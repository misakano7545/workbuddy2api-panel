package panel

// tasklive_test.go 钉住「实时任务」端点的三个行为：
//   - TTL 内复用缓存（轮询不该把上游当靶子打）；
//   - fresh=1 穿透缓存；
//   - 单账号失败只标在该行，不影响其它账号。
//
// 这三条都是「看板会不会变成压测 / 一个号挂了会不会整块空掉」的直接判据。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// liveTasksPanel 起一个假上游：/tasks 按 uid 返回不同任务表。
// 通过 header X-User-Id 区分账号（与真实链路一致）。
func liveTasksPanel(t *testing.T, calls *atomic.Int32, failUID string) *Panel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/tasks") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		uid := r.Header.Get("X-User-Id")
		if failUID != "" && uid == failUID {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"token expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
			{"task_code":"chat_5","title":"发起 5 次对话","target_count":5,"progress":{"current":2,"target":5},
			 "reward_credit":100,"accept_status":"accepted"},
			{"task_code":"first_buddy","title":"领取一只 Buddy","target_count":1,"progress":{"current":1,"target":1},
			 "reward_credit":300,"accept_status":"claimed"}]}}`))
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.HTTP, up.ChatBaseCN = srv.Client(), srv.URL
	p := pool.New("")
	p.Add(testRealmAuth(t, "cn-1", "cn"))
	p.Add(testRealmAuth(t, "cn-2", "cn"))
	return New(Config{Pool: p, Upstream: up})
}

func getLive(t *testing.T, p *Panel, url string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", url, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTasksLiveCountsAndCache(t *testing.T) {
	taskLive = &liveCache{} // 每个用例独立缓存
	var calls atomic.Int32
	p := liveTasksPanel(t, &calls, "")

	out := getLive(t, p, "/panel/api/tasks/live")
	accts, _ := out["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("accounts=%d want 2", len(accts))
	}
	first, _ := accts[0].(map[string]any)
	if first["pending"].(float64) != 1 || first["claimed"].(float64) != 1 {
		t.Fatalf("计数不对: %+v", first)
	}
	n1 := calls.Load()
	if n1 != 2 { // 两个账号各一次
		t.Fatalf("首轮上游调用=%d want 2", n1)
	}

	// 第二轮：TTL 内应直接复用，不再打上游
	out2 := getLive(t, p, "/panel/api/tasks/live")
	if out2["cached"] != true {
		t.Fatalf("TTL 内应命中缓存: %+v", out2)
	}
	if calls.Load() != n1 {
		t.Fatalf("缓存命中仍打了上游：%d -> %d", n1, calls.Load())
	}

	// fresh=1 穿透
	out3 := getLive(t, p, "/panel/api/tasks/live?fresh=1")
	if out3["cached"] != false {
		t.Fatalf("fresh=1 应穿透缓存: %+v", out3)
	}
	if calls.Load() <= n1 {
		t.Fatalf("fresh=1 未实际查询上游：%d", calls.Load())
	}
}

func TestTasksLivePerAccountFailureIsolated(t *testing.T) {
	taskLive = &liveCache{}
	var calls atomic.Int32
	p := liveTasksPanel(t, &calls, "cn-2")

	out := getLive(t, p, "/panel/api/tasks/live")
	accts, _ := out["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("一个号失败不该让整块空掉：accounts=%d", len(accts))
	}
	var okRow, badRow map[string]any
	for _, a := range accts {
		m := a.(map[string]any)
		if m["uid"] == "cn-2" {
			badRow = m
		} else {
			okRow = m
		}
	}
	if okRow == nil || okRow["error"] != nil {
		t.Fatalf("健康号不该有 error: %+v", okRow)
	}
	if badRow == nil || badRow["error"] == nil || badRow["error"] == "" {
		t.Fatalf("失败号应带 error: %+v", badRow)
	}
	if ts, ok := badRow["tasks"].([]any); ok && len(ts) != 0 {
		t.Fatalf("失败号不该有任务: %+v", badRow)
	}
}

func TestTasksLiveSkipsDisabledAccounts(t *testing.T) {
	taskLive = &liveCache{}
	var calls atomic.Int32
	p := liveTasksPanel(t, &calls, "")
	p.cfg.Pool.Disable("cn-1", "test")

	out := getLive(t, p, "/panel/api/tasks/live")
	accts, _ := out["accounts"].([]any)
	if len(accts) != 1 {
		t.Fatalf("禁用号不该出现在实时任务里：%d", len(accts))
	}
	if calls.Load() != 1 {
		t.Fatalf("禁用号不该打上游：calls=%d want 1", calls.Load())
	}
}
