package panel

import (
	"testing"
	"time"
)

// fresh=1 有下限：上一轮刚结束时连点「立即刷新」（或脚本反复带 fresh）仍回缓存，
// 否则每次都是每个启用账号一次上游 GET，把看板变成压测。过了下限才穿透。
func TestTasksLiveFreshHasFloor(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f)
	getLiveJSON(t, p, "/panel/api/tasks/live")
	for i := 0; i < 3; i++ {
		if out := getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1"); out["cached"] != true {
			t.Fatalf("下限内的 fresh=1 应回缓存: %+v", out)
		}
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("下限内的 fresh=1 不该打上游：calls=%d want 2", n)
	}

	old := taskLiveFreshFloor
	taskLiveFreshFloor = time.Nanosecond
	t.Cleanup(func() { taskLiveFreshFloor = old })
	time.Sleep(2 * time.Millisecond)
	if out := getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1"); out["cached"] != false {
		t.Fatalf("过了下限 fresh=1 应穿透: %+v", out)
	}
	if n := f.calls.Load(); n != 4 {
		t.Fatalf("calls=%d want 4", n)
	}
}
