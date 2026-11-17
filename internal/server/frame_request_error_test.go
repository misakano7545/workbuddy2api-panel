package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
)

// TestChatFirstFrameRequestErrorStopsRotation 请求本身的错误以「HTTP 200 + 首帧 error 帧」出现时
// 与 HTTP 4xx 同口径：换任何账号都是同一个拒绝，立即 400 + 对应 code，不再把轮转额度烧完后
// 以 503 no_healthy_account 收尾（客户端会把 503 当可重试，对必然失败的请求反复重发）。
func TestChatFirstFrameRequestErrorStopsRotation(t *testing.T) {
	for _, tc := range []struct {
		name, frame, wantCode string
	}{
		{"11129 只有 code", `{"error":{"message":"something else","code":11129}}`, "bad_params"},
		{"11129 中文文案", `{"error":{"message":"工具定义不合规"}}`, "bad_params"},
		{"11101", `{"error":{"message":"Unmarshal chat params failed: unexpected EOF"}}`, "bad_params"},
		{"11115", `{"error":{"message":"prompt is too long: 300000 tokens > 200000 maximum"}}`, "prompt_too_long"},
		{"11128 内容拦截", `{"error":{"message":"blocked by security policy","code":11128}}`, "content_blocked"},
		{"图片无效", `{"error":{"message":"Parse message failed: invalid image_url content"}}`, "image_invalid"},
		{"11148", `{"error":{"message":"tool calls and tool results do not match"}}`, "tool_call_mismatch"},
		{"11133", `{"error":{"message":"request parameters do not meet the current model requirements","code":11133}}`, "model_param_invalid"},
		{"11151", `{"error":{"message":"a message has empty content","code":11151}}`, "empty_content"},
		{"11133 真实措辞", `{"error":{"message":"the request parameters were rejected by the model provider","code":11133}}`, "model_param_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				mu.Lock()
				calls++
				mu.Unlock()
				return 200, "data: " + tc.frame + "\n\ndata: [DONE]\n\n", true
			})
			p := testPoolWith(
				&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
				&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
				&auth.Auth{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
			)
			h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute, PromptMode: "passthrough"})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body)
			}
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("响应不是 JSON 错误信封: %v: %s", err, rec.Body)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("error.code=%q, want %q (body=%s)", env.Error.Code, tc.wantCode, rec.Body)
			}
			if !strings.Contains(env.Error.Message, "error") {
				t.Errorf("error.message 应透传上游 error 帧原文: %q", env.Error.Message)
			}
			if calls != 1 {
				t.Errorf("上游被调用 %d 次, want 1（请求级错误不换号）", calls)
			}
			for _, uid := range []string{"a", "b", "c"} {
				if st, _ := p.Status(uid); st.Cooling || st.Disabled {
					t.Errorf("账号 %s 不应被罚: %+v", uid, st)
				}
			}
		})
	}
}

// TestProbeOnceStopsOnRequestLevelError 托管工具探测体错在请求本身（11129 工具定义不合规、
// 11128 内容拦截等）时换号必然同样被拒，不再逐个试满 webSearchPickTries 个账号；
// 账号级失败（5xx）仍照常换号。
func TestProbeOnceStopsOnRequestLevelError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantCalls  int
	}{
		{"11129", `{"code":11129,"msg":"invalid function call parameters"}`, 400, 1},
		{"11128", `{"code":11128,"msg":"blocked by security policy"}`, 400, 1},
		{"5xx 照常换号", `{"code":500,"msg":"boom"}`, 500, webSearchPickTries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				calls++
				return tc.status, tc.body, false
			})
			p := testPoolWith(
				&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
				&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
				&auth.Auth{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
			)
			h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
			if _, err := h.probeOnce(context.Background(), "cn", "glm-5.2", []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)); err == nil {
				t.Fatal("探测应失败")
			}
			if calls != tc.wantCalls {
				t.Errorf("上游被调用 %d 次, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// TestChatFirstFrameRequestErrorKeepsBinding 请求级错误与账号无关：账号仍然健康、前缀缓存还热，
// 首帧 error 帧回 400 时不解绑会话粘性（与 HTTP 路径的 11115 / 图片无效 / 11148 同待遇），
// 否则客户端修好请求重试时会被分到一个缓存全冷的号上。
func TestChatFirstFrameRequestErrorKeepsBinding(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, "data: {\"error\":{\"message\":\"prompt is too long: 300000 tokens > 200000 maximum\"}}\n\ndata: [DONE]\n\n", true
	})
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"deep1", "deep2"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "deep1", AccessToken: "at-deep1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "deep2", AccessToken: "at-deep2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-9", "deep1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[],"metadata":{"conversation_id":"conv-9"}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body)
	}
	if calls["Bearer at-deep1"] != 1 || calls["Bearer at-deep2"] != 0 {
		t.Errorf("calls=%v want 只打粘性号一次", calls)
	}
	if uid, ok := st.lastUID("conv-9"); !ok || uid != "deep1" {
		t.Errorf("会话绑应保留 deep1，got %q ok=%v", uid, ok)
	}
}

// TestChatFirstFrameNonRequestLevelErrorStillRotates 对照：不在请求级表里的首帧 error 帧
// （code 11130 落 ErrClient）仍然换号，三个账号各打一次后才以 503 收尾——止损只针对请求本身的错误。
func TestChatFirstFrameNonRequestLevelErrorStillRotates(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, "data: {\"error\":{\"message\":\"something odd\",\"code\":11130}}\n\ndata: [DONE]\n\n", true
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable || calls != 3 {
		t.Errorf("code=%d calls=%d body=%s, want 503 after 3 upstream calls", rec.Code, calls, rec.Body)
	}
}
