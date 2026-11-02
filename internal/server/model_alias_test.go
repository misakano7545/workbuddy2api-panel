package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// aliasTestHandler 构造一个带 aliases 快照的 handler，并返回出站 body 捕获口。
//
// 出站 body 由 fake transport 读 r.Body 捕获（prepareBody 已强制 stream + 归一），
// 断言点与 TestCustomModeFingerprintSanitizePreserved 同一位置。
func aliasTestHandler(t *testing.T, aliasEntries []string, hidden []string) (*Handler, *[]byte) {
	t.Helper()
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
		Live: livecfg.New(livecfg.Snapshot{
			HiddenModels: livecfg.NormalizeHiddenModels(hidden),
			ModelAliases: livecfg.NormalizeModelAliases(aliasEntries),
		}),
	})
	return h, &sentBody
}

// TestModelAliasRewritesOutboundBody 映射命中：出站 body 的 model 换成真实名，
// 响应（聚合体）的 model 回显客户端原发名（issue #30）。
func TestModelAliasRewritesOutboundBody(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-gpt","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(*sent, &out); err != nil {
		t.Fatalf("出站 body 解析失败: %v (%s)", err, *sent)
	}
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2（映射后真实名）", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v (%s)", err, rec.Body)
	}
	if got := resp["model"]; got != "my-gpt" {
		t.Errorf("响应 model = %v, want my-gpt（回显客户端原发名）", got)
	}
}

// TestModelAliasStreamEcho 流式：帧内 model 覆写回客户端原发名。
func TestModelAliasStreamEcho(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2", got)
	}
	// 上游帧里 model 是 glm-5.2（sseOK 固定值），出站帧必须已改回 my-gpt。
	body := rec.Body.String()
	if strings.Contains(body, `"model":"glm-5.2"`) {
		t.Errorf("流式帧仍带真实模型名，回显未生效:\n%s", body)
	}
	if !strings.Contains(body, `"model":"my-gpt"`) {
		t.Errorf("流式帧缺客户端原发名:\n%s", body)
	}
}

// TestModelAliasCannotBypassHidden 别名目标受黑名单约束（映射在 hidden 检查之前
// 完成，检查的是真实名）。
func TestModelAliasCannotBypassHidden(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, []string{"glm-5.2"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-gpt","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400（映射目标被隐藏）", rec.Code, rec.Body)
	}
	if len(*sent) != 0 {
		t.Errorf("被隐藏的映射目标不应打上游，实际出站 body=%s", *sent)
	}
}

// TestModelAliasLookup 查找口径：大小写不敏感；域前缀键只在该域生效；
// 未配置的表（nil）不命中。
func TestModelAliasLookup(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		Live: livecfg.New(livecfg.Snapshot{
			ModelAliases: livecfg.NormalizeModelAliases([]string{"My-GPT=glm-5.2", "cn:opus=global:gpt-5.4"}),
		}),
	})
	if got, ok := h.modelAlias("cn", "my-gpt"); !ok || got != "glm-5.2" {
		t.Errorf("大小写不敏感查找失败: %q ok=%v", got, ok)
	}
	if got, ok := h.modelAlias("CN", "MY-GPT"); !ok || got != "glm-5.2" {
		t.Errorf("realm 大写归一化失败: %q ok=%v", got, ok)
	}
	if _, ok := h.modelAlias("global", "opus"); ok {
		t.Error("cn: 前缀键不应在 global 域命中")
	}
	if got, ok := h.modelAlias("cn", "opus"); !ok || got != "global:gpt-5.4" {
		t.Errorf("跨域指向值应原样返回: %q ok=%v", got, ok)
	}
	if _, ok := h.modelAlias("cn", ""); ok {
		t.Error("空模型名不应命中")
	}

	// 无表（nil 快照）→ 不命中
	h2 := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	if _, ok := h2.modelAlias("cn", "my-gpt"); ok {
		t.Error("未配置映射时不应命中")
	}
}

// TestModelAliasWildcard 通配键：`claude-*` 命中未知型号，精确键仍优先。
func TestModelAliasWildcard(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		Live: livecfg.New(livecfg.Snapshot{
			ModelAliases: livecfg.NormalizeModelAliases([]string{
				"claude-*=glm-5.2", "cn:claude-sonnet-4-5=global:kimi-k2.7", "gpt-?=glm-5.3-flash",
			}),
		}),
	})
	// 通配命中：Claude Code 发什么都不用一条条配
	if got, ok := h.modelAlias("cn", "claude-sonnet-4-6"); !ok || got != "glm-5.2" {
		t.Errorf("通配未命中: %q ok=%v", got, ok)
	}
	// 精确键优先于通配
	if got, ok := h.modelAlias("cn", "claude-sonnet-4-5"); !ok || got != "global:kimi-k2.7" {
		t.Errorf("精确键应压过通配: %q ok=%v", got, ok)
	}
	// ? 匹配单字节
	if got, ok := h.modelAlias("global", "gpt-5"); !ok || got != "glm-5.3-flash" {
		t.Errorf("? 通配未命中: %q ok=%v", got, ok)
	}
	// 不认识的名字仍然不命中（不猜、不悄悄换模型）
	if _, ok := h.modelAlias("cn", "something-else"); ok {
		t.Error("未配置的名字不应被兜底")
	}
}

// TestCountTokensEndpoint Claude Code 的计数端点：要 200 且给个正数，图片不能被算成
// base64 长度（一张截图的 base64 有几十万字符，按字符算会直接报上下文爆满）。
func TestCountTokensEndpoint(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	big := strings.Repeat("A", 400000) // 约一张 png 的 base64
	body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"你好，帮我看下这个问题"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}`
	for _, path := range []string{"/v1/messages/count_tokens", "/messages/count_tokens"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body)
		}
		var out struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 响应解析失败: %v", path, err)
		}
		if out.InputTokens < 10 || out.InputTokens > 5000 {
			t.Errorf("%s input_tokens=%d，超出粗估合理区间（图片按张算应约 1600+）", path, out.InputTokens)
		}
	}
}

// TestModelAliasUnmappedZeroChange 未命中映射的请求：出站 body 与响应都保持原样
// （别名字段存在也不影响未映射模型）。
func TestModelAliasUnmappedZeroChange(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("未映射模型出站 model = %v, want glm-5.2", got)
	}
}
