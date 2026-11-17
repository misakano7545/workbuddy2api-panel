package panel

import "testing"

// 失败号每轮只请求一次：上游 401（token 失效）时再换默认口径重试，在封号潮里一批号
// 同时失效就会把一轮的请求数翻倍，而且换口径不会让 401 变成功。
func TestTasksLiveFailedAccountIsNotRetried(t *testing.T) {
	f := &liveFake{failUID: "cn-2"}
	p := newLivePanel(t, f)

	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if bad := liveRowOf(out, "cn-2"); bad == nil || bad["error"] == nil {
		t.Fatalf("失败号应带 error: %+v", bad)
	}
	if ok := liveRowOf(out, "cn-1"); ok == nil || ok["error"] != nil {
		t.Fatalf("健康号不该受影响: %+v", ok)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("每个号每轮恰好一次请求：calls=%d want 2", n)
	}
}
