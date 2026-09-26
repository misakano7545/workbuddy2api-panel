package panel

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestRunGrowthQueueNowUnguarded 排程入口在未接线（无上游）时安全返回，不 panic。
func TestRunGrowthQueueNowUnguarded(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New("")})
	total, msg := p.RunGrowthQueueNow()
	if total != 0 || msg == "" {
		t.Errorf("total=%d msg=%q want 0 + 原因文案", total, msg)
	}
}

// TestRunGrowthQueueNowBusy 队列执行中再启动：排程与手动入口共用队列状态，不重复启动。
func TestRunGrowthQueueNowBusy(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New(""), Upstream: &upstream.Client{}})
	q := p.queue()
	q.mu.Lock()
	q.running = true
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
	}()
	if total, msg := p.RunGrowthQueueNow(); total != 0 || msg != "队列正在执行中" {
		t.Errorf("total=%d msg=%q want 0 + 队列正在执行中", total, msg)
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
