package panel

import "testing"

// 全部领完的号不带任务表：生产两百上下个号里绝大多数是这种，带上只是每轮白白序列化
// 几百 KB、前端白白重画几千行。计数仍要有，汇总才对得上。
func TestTasksLiveFullyClaimedOmitsTasks(t *testing.T) {
	f := &liveFake{doneUID: "cn-2"}
	p := newLivePanel(t, f)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	done, busy := liveRowOf(out, "cn-2"), liveRowOf(out, "cn-1")
	if done["claimed"].(float64) != 1 || done["pending"].(float64) != 0 {
		t.Fatalf("已领完的号计数应仍在: %+v", done)
	}
	if _, has := done["tasks"]; has {
		t.Fatalf("已领完的号不该带任务表: %+v", done)
	}
	if ts, _ := busy["tasks"].([]any); len(ts) != 2 {
		t.Fatalf("还有待办的号应带完整任务表: %+v", busy)
	}
}
