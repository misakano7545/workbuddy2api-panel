package panel

import (
	"testing"
	"time"
)

// 一轮跑得比请求愿意等的久（账号多、个别上游挂住，单次最长 120s）：请求要在上限内
// 带着 refreshing=true 返回，后到的请求（含 fresh）不能再叠一轮，上游放行后下一次
// 请求拿到完整快照。上游原实现让首个请求一直等到整轮结束，后到的请求 30s 后回 503。
func TestTasksLiveSlowRoundDoesNotBlockOrStack(t *testing.T) {
	old, oldFloor := taskLiveWait, taskLiveFreshFloor
	taskLiveWait, taskLiveFreshFloor = 50*time.Millisecond, 0
	t.Cleanup(func() { taskLiveWait, taskLiveFreshFloor = old, oldFloor })

	f := &liveFake{block: make(chan struct{})}
	p := newLivePanel(t, f)

	start := time.Now()
	out := getLiveJSON(t, p, "/panel/api/tasks/live")
	if time.Since(start) > 5*time.Second {
		t.Fatalf("请求被慢轮次拖住了：%v", time.Since(start))
	}
	if out["refreshing"] != true {
		t.Fatalf("一轮未完成时应报 refreshing：%+v", out)
	}
	if out["cached"] != true {
		t.Fatalf("等待超时后回的是上一份快照，cached 应为 true：%+v", out)
	}
	getLiveJSON(t, p, "/panel/api/tasks/live")
	getLiveJSON(t, p, "/panel/api/tasks/live?fresh=1")

	close(f.block)
	deadline := time.Now().Add(5 * time.Second)
	for {
		out = getLiveJSON(t, p, "/panel/api/tasks/live")
		if out["refreshing"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("放行后一轮仍未结束：%+v", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if accts, _ := out["accounts"].([]any); len(accts) != 2 {
		t.Fatalf("放行后应拿到完整快照：%+v", out)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("慢轮次期间不该叠第二轮：calls=%d want 2", n)
	}
}
