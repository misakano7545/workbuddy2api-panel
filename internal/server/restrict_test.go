// restrict_test.go 11140 风控连坐保护的两道闸：
//  1. 命中 11140 就地停手（同一请求不再挨个换号去打）；
//  2. 同一域 15 分钟窗内 2 个不同账号被限制 → 暂停该域派发（期间不打上游，503 + Retry-After）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

const restrictBody = `{"code":11140,"msg":"账号存在异常行为，已被限制使用","requestId":"aaaabbbbccccdddd"}`

func postChatModel(h *Handler, model string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	return rec
}

func postChatRestricted(h *Handler) *httptest.ResponseRecorder { return postChatModel(h, "glm-5.2") }

// 单号反复命中不凑数；hold<=0 永不暂停；两个域互不牵连。
func TestRestrictionGate(t *testing.T) {
	var g restrictionGate
	if g.note("cn", "a", time.Minute, 0) || g.note("cn", "a", time.Minute, 0) {
		t.Fatal("同一个账号重复命中不该凑成第二个号")
	}
	if g.note("global", "b", time.Minute, 0) {
		t.Fatal("另一个域的命中不该凑进 cn 的计数")
	}
	if !g.note("cn", "b", time.Minute, 0) {
		t.Fatal("同域窗口内第 2 个不同账号被限制应触发暂停")
	}
	if d := g.remaining("cn"); d <= 0 || d > time.Minute {
		t.Fatalf("cn remaining=%v want (0, 1m]", d)
	}
	if d := g.remaining("global"); d != 0 {
		t.Fatalf("cn 的暂停不该波及 global, remaining=%v", d)
	}

	var off restrictionGate
	for _, uid := range []string{"a", "b", "c"} {
		if off.note("cn", uid, 0, 0) {
			t.Fatal("hold=0 时不该暂停")
		}
	}
}

// 不带 code 11140 的 "request illegal"：不是账号风控限制，照常换号，也不计入连坐。
func TestChatRequestIllegalWithoutCodeKeepsRotating(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, `{"code":10001,"msg":"request illegal"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "i1", AccessToken: "at-i1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i2", AccessToken: "at-i2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: time.Minute, MaxRotate: 3})
	postChatModel(h, "glm-5.2")
	if calls != 2 {
		t.Fatalf("calls=%d want 2（非 11140 不该停轮转）", calls)
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("非 11140 不该凑成连坐暂停, remaining=%v", d)
	}
}

// 流式首帧就是 11140 文书（HTTP 200 内嵌 {code,msg}）：首帧否决之后同样停手、禁用该号。
func TestChatStreamHeadRestrictionStopsRotation(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, "data: " + restrictBody + "\n\ndata: [DONE]\n\n", true
	})
	p := testPoolWith(
		&auth.Auth{UID: "v1", AccessToken: "at-v1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "v2", AccessToken: "at-v2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: time.Minute, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if calls != 1 {
		t.Fatalf("calls=%d want 1（首帧 11140 也不许换号）body=%s", calls, rec.Body)
	}
	if _, _, _, n, _ := p.CountsDetailed(); n != 1 {
		t.Fatalf("被限制的号应禁用, disabled=%d", n)
	}
}

// 端到端：上游一直回 11140 文书时，每个请求只打一次上游（不轮转）且禁用该号；第二个被
// 限制的号把该域推进暂停，之后该域请求直接 503、不打上游，另一个域照常放行。hold=0 时
// 仍然命中即停手，只是不暂停。
func TestChatStopsRotationOnRestrictedAccount(t *testing.T) {
	for _, hold := range []time.Duration{30 * time.Minute, 0} {
		calls := 0
		up := newFakeUpstream(t, func(string) (int, string, bool) {
			calls++
			return 403, restrictBody, false
		})
		p := testPoolWith(
			&auth.Auth{UID: "r1", AccessToken: "at-r1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "r2", AccessToken: "at-r2", ExpiresAt: 9999999999},
			&auth.Auth{UID: "r3", AccessToken: "at-r3", ExpiresAt: 9999999999},
		)
		h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: hold, MaxRotate: 3})

		postChatModel(h, "glm-5.2")
		if calls != 1 {
			t.Fatalf("hold=%v: calls=%d want 1（命中 11140 不许换号继续打）", hold, calls)
		}
		postChatModel(h, "glm-5.2")
		if calls != 2 {
			t.Fatalf("hold=%v: calls=%d want 2（每个请求各打一次）", hold, calls)
		}
		if _, _, _, n, _ := p.CountsDetailed(); n != 2 {
			t.Fatalf("hold=%v: 被限制的号应禁用, disabled=%d", hold, n)
		}
		rec := postChatModel(h, "glm-5.2")
		if hold == 0 {
			if h.restrict.remaining("cn") != 0 || calls != 3 {
				t.Fatalf("hold=0 时不该暂停, calls=%d", calls)
			}
			continue
		}
		if calls != 2 || rec.Code != 503 || !strings.Contains(rec.Body.String(), "account_restricted_hold") ||
			rec.Header().Get("Retry-After") == "" {
			t.Fatalf("暂停期内应 503 + Retry-After 且不打上游: calls=%d code=%d body=%s", calls, rec.Code, rec.Body)
		}
		if rec := postChatModel(h, "global:glm-5.2"); strings.Contains(rec.Body.String(), "account_restricted_hold") {
			t.Fatalf("cn 的暂停不该挡 global 请求: %s", rec.Body)
		}
	}
}
