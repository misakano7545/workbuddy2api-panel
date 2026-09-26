package panel

import (
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
