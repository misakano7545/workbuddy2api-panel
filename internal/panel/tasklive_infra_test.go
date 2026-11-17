package panel

import "testing"

// 夹具自检：假上游 + 实时任务端点能跑通一轮，两个国内号各被查一次。
func TestTasksLiveFakeSmoke(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f)
	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if liveRowOf(out, "cn-1") == nil || liveRowOf(out, "cn-2") == nil || f.calls.Load() != 2 {
		t.Fatalf("out=%v calls=%d", out, f.calls.Load())
	}
}
