package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatModelBlockedReportsModelUnavailable 池里号都在、只是每个号都对该模型处于
// 模型级冷却（11102/6004）时，客户端必须看到 400 model_unavailable + 本地调度信息，
// 而不是「没有可用账号」的 503——前者该换模型，后者该等，处置相反（吸收上游 44d6e04）。
func TestChatModelBlockedReportsModelUnavailable(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	p.BlockModelBackoff("u1", "glm-5.2", "11102 model [glm-5.2] service info not found")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("模型全池冷却时不应打上游（选号就该失败），auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400 model_unavailable)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "model_unavailable") {
		t.Errorf("body 应带 code=model_unavailable：%s", body)
	}
	if !strings.Contains(body, "model_blocked: 1 account") {
		t.Errorf("gateway_hint 应带被挡账号数：%s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("不得退回 no_healthy_account 文案：%s", body)
	}
}

// TestChatNoAccountsStillNoHealthyAccount 池子真没号（无模型冷却）时口径不变：
// 仍是 503 + no_healthy_account，绝不能冒充模型不可用（防止 ModelBlocked 误报）。
func TestChatNoAccountsStillNoHealthyAccount(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	p.Disable("u1", "test") // 唯一账号被禁用 → 真没号
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("无可用账号时不应打上游，auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503 no_healthy_account)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no_healthy_account") {
		t.Errorf("body 应保持 no_healthy_account：%s", body)
	}
	if strings.Contains(body, "model_unavailable") {
		t.Errorf("真没号不得报成模型不可用：%s", body)
	}
}
