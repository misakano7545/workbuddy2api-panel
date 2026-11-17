package panel

import "testing"

// 国际号只走默认口径，不带小程序头：该头对国际账号没有验证过的契约（单号任务弹窗
// 同样明确不探 MP）。国内号仍走小程序口径。
func TestTasksLiveGlobalAccountSkipsMiniProgramHeader(t *testing.T) {
	f := &liveFake{}
	p := newLivePanel(t, f, testRealmAuth(t, "gl-1", "global"))

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if row := liveRowOf(out, "gl-1"); row == nil || row["realm"] != "global" {
		t.Fatalf("国际号应出现在结果里：%+v", out["accounts"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.mpCalls["gl-1"]; got != "" {
		t.Fatalf("国际号不该带小程序口径头，实际 X-Client-Platform=%q", got)
	}
	if got := f.mpCalls["cn-1"]; got == "" {
		t.Fatalf("国内号应走小程序口径，实际头为空")
	}
}
