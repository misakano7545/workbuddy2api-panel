// restrict_test.go 11140 风控连坐保护的两道闸：
//  1. 命中 11140 就地停手（同一请求不再挨个换号去打）；
//  2. 15 分钟窗内 2 个不同账号被限制 → 全池暂停派发（期间不打上游，503 + Retry-After）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

const restrictBody = `{"code":11140,"msg":"账号存在异常行为，已被限制使用","requestId":"aaaabbbbccccdddd"}`

func postChatRestricted(h *Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	return rec
}

// TestRestrictionGateNeedsTwoAccounts 单号反复 11140 归账号自己的禁用，不触发全池暂停；
// 同一 uid 重复上报不许把计数刷成「两个号」。
func TestRestrictionGateNeedsTwoAccounts(t *testing.T) {
	var g restrictionGate
	if g.note("a", time.Minute) {
		t.Fatal("只有 1 个账号被限制时不该暂停全池")
	}
	if g.note("a", time.Minute) {
		t.Fatal("同一个账号重复命中不该凑成第二个号")
	}
	if d := g.remaining(); d != 0 {
		t.Fatalf("remaining=%v want 0", d)
	}
	if !g.note("b", time.Minute) {
		t.Fatal("窗口内第 2 个不同账号被限制应触发全池暂停")
	}
	if d := g.remaining(); d <= 0 || d > time.Minute {
		t.Fatalf("remaining=%v want (0, 1m]", d)
	}
	g.reset()
	if d := g.remaining(); d != 0 {
		t.Fatalf("reset 后 remaining=%v want 0", d)
	}
}

// TestRestrictionGateHoldDisabled hold=0（upstream.restriction_hold_seconds: 0）时
// 只保留「命中即停手」，永不暂停全池。
func TestRestrictionGateHoldDisabled(t *testing.T) {
	var g restrictionGate
	for _, uid := range []string{"a", "b", "c"} {
		if g.note(uid, 0) {
			t.Fatal("hold=0 时不该暂停")
		}
	}
	if d := g.remaining(); d != 0 {
		t.Fatalf("remaining=%v want 0", d)
	}
}

// TestChatStopsRotationOnRestrictedAccount 端到端：上游一直回 11140 文书时，
// 每个请求只打一次上游（不轮转），第二个被限制的号把全池推进暂停，之后的请求
// 直接失败、根本不打上游。
func TestChatStopsRotationOnRestrictedAccount(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, restrictBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "r1", AccessToken: "at-r1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "r2", AccessToken: "at-r2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: 30 * time.Minute})

	// 第 1 个请求：1 个号被限制 → 就地停手。
	if rec := postChatRestricted(h); rec.Code == 0 {
		t.Fatal("no response")
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1（命中 11140 不许换号继续打）", calls)
	}
	if d := h.restrict.remaining(); d != 0 {
		t.Fatalf("只有 1 个号被限制时不该暂停全池, remaining=%v", d)
	}

	// 第 2 个请求：另一个号也被限制 → 触发全池暂停。
	postChatRestricted(h)
	if calls != 2 {
		t.Fatalf("calls=%d want 2（每个请求各打一次）", calls)
	}
	if d := h.restrict.remaining(); d <= 0 {
		t.Fatalf("窗口内第 2 个号被限制应触发全池暂停, remaining=%v", d)
	}

	// 第 3 个请求：暂停期内快速失败，不打上游。
	rec := postChatRestricted(h)
	if calls != 2 {
		t.Fatalf("暂停期内不该再打上游, calls=%d", calls)
	}
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "account_restricted_hold") {
		t.Errorf("应给出 account_restricted_hold 业务码: %s", body)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("暂停期内应带 Retry-After，客户端才知道等多久")
	}
}

// TestChatRestrictionHoldZeroKeepsStopping 配置 hold=0 时仍然「命中即停手」：
// 只是不再全池暂停。少掉的这一点是运维主动选的，不能顺手把停手也关掉。
func TestChatRestrictionHoldZeroKeepsStopping(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, restrictBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "z1", AccessToken: "at-z1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "z2", AccessToken: "at-z2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: 0})

	for i := 0; i < 2; i++ {
		postChatRestricted(h)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2（每个请求仍只打一次）", calls)
	}
	if d := h.restrict.remaining(); d != 0 {
		t.Fatalf("hold=0 时不该暂停全池, remaining=%v", d)
	}
}
