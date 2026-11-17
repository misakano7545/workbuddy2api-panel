package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// liveFake 实时任务测试的假上游：按 X-User-Id 区分账号，记录每次请求的口径头。
type liveFake struct {
	calls   atomic.Int32
	mu      sync.Mutex
	mpCalls map[string]string // uid -> X-Client-Platform
	failUID string
	doneUID string        // 这个号的任务全部已领
	block   chan struct{} // 非 nil 时请求阻塞到它被关闭
}

func (f *liveFake) handler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/tasks") {
		http.NotFound(w, r)
		return
	}
	f.calls.Add(1)
	uid := r.Header.Get("X-User-Id")
	f.mu.Lock()
	if f.mpCalls == nil {
		f.mpCalls = map[string]string{}
	}
	f.mpCalls[uid] = r.Header.Get("X-Client-Platform")
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	if uid == f.failUID {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"token expired"}`))
		return
	}
	pending := `{"task_code":"chat_5","title":"发起 5 次对话","target_count":5,"progress":{"current":2,"target":5},
			 "reward_credit":100,"accept_status":"accepted"},`
	if uid == f.doneUID {
		pending = ""
	}
	_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[` + pending + `
			{"task_code":"first_buddy","title":"领取一只 Buddy","target_count":1,"progress":{"current":1,"target":1},
			 "reward_credit":300,"accept_status":"claimed"}]}}`))
}

// newLivePanel 起假上游和一个带 cn-1 / cn-2 两个国内号的面板（global 域也指向假上游）。
func newLivePanel(t *testing.T, f *liveFake, extra ...*auth.Auth) *Panel {
	t.Helper()
	useRealmForTest(t)
	taskLive = &liveCache{} // 每个用例独立缓存
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.HTTP, up.ChatBaseCN, up.ChatBaseGlobal = srv.Client(), srv.URL, srv.URL
	p := pool.New("")
	p.Add(testRealmAuth(t, "cn-1", "cn"))
	p.Add(testRealmAuth(t, "cn-2", "cn"))
	for _, a := range extra {
		p.Add(a)
	}
	return New(Config{Pool: p, Upstream: up})
}

func getLiveJSON(t *testing.T, p *Panel, url string) map[string]any {
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

func liveRowOf(out map[string]any, uid string) map[string]any {
	accts, _ := out["accounts"].([]any)
	for _, a := range accts {
		if m := a.(map[string]any); m["uid"] == uid {
			return m
		}
	}
	return nil
}
