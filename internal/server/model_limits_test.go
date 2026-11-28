package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/ratelimit"
)

// newModelLimitHandler 造一个带 ratelimit tracker 的 handler。
// 用量/账号固定成可控快照，Path 指向 t.TempDir() 避免碰真实策略文件或空路径报错。
func newModelLimitHandler(t *testing.T, p ratelimit.Policy) (*Handler, *ratelimit.Tracker) {
	t.Helper()
	tr := ratelimit.NewTracker(ratelimit.TrackerConfig{
		Path: filepath.Join(t.TempDir(), "model_limits.json"),
		Deps: ratelimit.Deps{
			// 一个可路由的 cn 账号 + 两条超额用量：让「额度耗尽」真的成立，
			// 也让窗口边界有 FirstAt 可算 Retry-After。
			Usage: func(time.Duration) []ratelimit.UsageRow {
				t := time.Now().Add(-23 * time.Hour)
				return []ratelimit.UsageRow{
					{Realm: "cn", UID: "u1", Model: "model-a", Tokens: 2, Requests: 1, FirstAt: t},
					{Realm: "cn", UID: "u1", Model: "model-b", Tokens: 2, Requests: 1, FirstAt: t},
				}
			},
			Accounts: func() []ratelimit.Account {
				return []ratelimit.Account{{UID: "u1", Realm: "cn"}}
			},
			Realms:   func() []string { return []string{"cn"} },
			Rates:    func(string) map[string]string { return map[string]string{} },
			Models:   func(string) []string { return nil },
			Fallback: func() map[string]string { return nil },
			Now:      time.Now,
		},
	})
	if _, err := tr.Save(p); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	pool := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: pool, MaxRotate: 3, ModelLimits: tr})
	return h, tr
}

func doModelLimitChat(h *Handler, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

func manLimit(realm, model string, token int64, hit string, chain ...string) ratelimit.ModelLimit {
	return ratelimit.ModelLimit{
		Realm: realm, Model: model, Source: ratelimit.SourceManual,
		TokenQuotaPerAccount: token, OnHit: hit, Chain: chain,
	}
}

// follow 默认策略：就算配额推算出来已用满，也不该本地拦（上游才是权威）。
func TestModelLimitsFollowDoesNotBlock(t *testing.T) {
	up := &modelAwareUpstream{reply: []struct {
		status int
		body   string
	}{{200, okChatSSE}}}
	h, _ := newModelLimitHandler(t, ratelimit.DefaultPolicy())
	h.cfg.Upstream = up.client()
	rec := doModelLimitChat(h, "model-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("follow 默认策略不应拦：status=%d body=%s", rec.Code, rec.Body)
	}
	if got := up.seen(); len(got) != 1 || got[0] != "model-a" {
		t.Fatalf("follow 不应改模型：上游收到 %v", got)
	}
}

// hold：手设 0 额度 + on_hit=hold → 429 + code + Retry-After，且不打上游。
func TestModelLimitsHoldReturns429WithRetryAfter(t *testing.T) {
	up := &modelAwareUpstream{}
	h, _ := newModelLimitHandler(t, ratelimit.Policy{
		Overrides: []ratelimit.ModelLimit{manLimit("cn", "model-a", 1, ratelimit.OnHitHold)},
	})
	h.cfg.Upstream = up.client()
	rec := doModelLimitChat(h, "model-a")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("hold 应回 429，得到 %d: %s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("429 body 不是 JSON: %v / %s", err, rec.Body)
	}
	e, _ := obj["error"].(map[string]any)
	if e["code"] != "model_limit_hold" {
		t.Fatalf("错误码应为 model_limit_hold，得到 %v", e["code"])
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("429 应带 Retry-After")
	}
	if len(up.seen()) != 0 {
		t.Fatalf("hold 不该打上游，上游却收到 %v", up.seen())
	}
}

// degrade：手设额度 + 链 → 出站 body 顶层 model 改成目标。
func TestModelLimitsDegradeRewritesOutboundModel(t *testing.T) {
	up := &modelAwareUpstream{reply: []struct {
		status int
		body   string
	}{{200, okChatSSE}}}
	h, _ := newModelLimitHandler(t, ratelimit.Policy{
		Overrides: []ratelimit.ModelLimit{manLimit("cn", "model-a", 1, ratelimit.OnHitDegrade, "model-b")},
	})
	h.cfg.Upstream = up.client()
	rec := doModelLimitChat(h, "model-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("degrade 后应成功，得到 %d: %s", rec.Code, rec.Body)
	}
	got := up.seen()
	if len(got) != 1 || got[0] != "model-b" {
		t.Fatalf("出站模型应为链首 model-b，得到 %v", got)
	}
}

// 多级降级：A 超限 → B 也超限 → C。每级都复查 Decide，最多 3 跳。
func TestModelLimitsMultiHopDegrade(t *testing.T) {
	up := &modelAwareUpstream{reply: []struct {
		status int
		body   string
	}{{200, okChatSSE}}}
	h, _ := newModelLimitHandler(t, ratelimit.Policy{
		Overrides: []ratelimit.ModelLimit{
			manLimit("cn", "model-a", 1, ratelimit.OnHitDegrade, "model-b", "model-c"),
			manLimit("cn", "model-b", 1, ratelimit.OnHitDegrade, "model-c"),
		},
	})
	h.cfg.Upstream = up.client()
	rec := doModelLimitChat(h, "model-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("多级降级应成功，得到 %d: %s", rec.Code, rec.Body)
	}
	got := up.seen()
	if len(got) != 1 || got[0] != "model-c" {
		t.Fatalf("应连降两级到 model-c，得到 %v", got)
	}
}
