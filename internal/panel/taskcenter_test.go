package panel

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestRunGrowthQueueNowUnguarded 排程入口在未接线（无上游）时安全返回，不 panic。
func TestRunGrowthQueueNowUnguarded(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New("")})
	total, msg := p.RunGrowthQueueNow()
	if total != 0 || msg == "" {
		t.Errorf("total=%d msg=%q want 0 + 原因文案", total, msg)
	}
}

// TestRunGrowthQueueNowBusy 账号被其它任务动作占用（per-account 锁）时，排程轮次
// 不重复启动该账号的后台作业——与手动/队列入口共用同一把锁。
func TestRunGrowthQueueNowBusy(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New("")})
	p.cfg.AutoTasksEnabled = func() bool { return true }
	p.cfg.Pool.Add(&auth.Auth{UID: "busy-1", AccessToken: "t"})
	if err := p.StartTaskJobs(context.Background(), filepath.Join(t.TempDir(), "task-jobs.json")); err != nil {
		t.Fatal(err)
	}
	defer p.StopTaskJobs()
	if !p.tryLockAccount("busy-1") {
		t.Fatal("占锁失败")
	}
	defer p.unlockAccount("busy-1")
	if total, _ := p.RunGrowthQueueNow(); total != 0 {
		t.Errorf("锁被占用时不应启动后台作业：total=%d", total)
	}
	if jobs := p.ListTaskJobs(); len(jobs) != 0 {
		t.Errorf("忙碌账号不应产生作业：%+v", jobs)
	}
}

// TestRunQueueItemsSummaryAndNotify 一轮跑完的汇总文案与推送开关：
// 排程轮次（notify=true）推一条，手动轮次（false）只落日志。
func TestRunQueueItemsSummaryAndNotify(t *testing.T) {
	p := New(Config{Version: "test"})
	var got string
	p.SetNotifier(func(s string) { got = s })
	q := p.queue()
	q.mu.Lock()
	q.seq = 3
	q.running = true
	q.items = []queueItem{
		{UID: "u1", Code: "Model_chat", Status: "done", Credit: 100, Energy: 5},
		{UID: "u2", Code: "black_cat", Status: "skipped"},
		{UID: "u2", Code: "Expert_5", Status: "error"},
	}
	items := q.items
	q.mu.Unlock()

	p.runQueueItems(nil, items, 1, true) // 空账号组：只走收尾（汇总 + 推送）
	for _, want := range []string{"第 3 轮 3 项", "完成 1 / 跳过 1 / 失败 1", "领奖 100 分 +5 能", "失败项 Expert_5"} {
		if !strings.Contains(got, want) {
			t.Errorf("汇总 %q 缺少 %q", got, want)
		}
	}
	if q.isRunning() {
		t.Error("轮次结束后 running 应为 false")
	}

	got = ""
	q.mu.Lock()
	q.running = true
	q.mu.Unlock()
	p.runQueueItems(nil, items, 1, false)
	if got != "" {
		t.Errorf("手动轮次不应推送：%q", got)
	}
}
