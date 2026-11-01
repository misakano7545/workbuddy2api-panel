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

// TestModelAliasWildcard 通配键：`*`/`?` 命中、精确键优先于通配、同域更长的模式优先。
// 裸名模式被 NormalizeModelAliases 摊成 cn:/global: 两条，两域都要命中。
func TestModelAliasWildcard(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		Live: livecfg.New(livecfg.Snapshot{
			ModelAliases: livecfg.NormalizeModelAliases([]string{
				"claude-*=glm-5.2",
				"cn:claude-*-pro=deepseek-v4-pro",
				"my-gpt=glm-5.3-flash",
			}),
		}),
	})
	cases := []struct{ realm, bare, want string }{
		{"cn", "claude-sonnet-4-5", "glm-5.2"},
		{"global", "CLAUDE-3-7-SONNET", "glm-5.2"},     // 大小写不敏感；裸名模式在两域都生效
		{"cn", "claude-opus-4-pro", "deepseek-v4-pro"}, // 同域更长的模式压过 claude-*
		{"cn", "my-gpt", "glm-5.3-flash"},              // 精确键优先于通配
	}
	for _, c := range cases {
		if got, ok := h.modelAlias(c.realm, c.bare); !ok || got != c.want {
			t.Errorf("modelAlias(%q,%q)=%q ok=%v want %q", c.realm, c.bare, got, ok, c.want)
		}
	}
	if got, ok := h.modelAlias("global", "claude-opus-4-pro"); !ok || got != "glm-5.2" {
		t.Errorf("global 域只该命中裸名模式 claude-* → glm-5.2（cn: 前缀的通配键不外溢），实际 %q ok=%v", got, ok)
	}
	if !wildcardMatch("a?c", "abc") || wildcardMatch("a?c", "ac") {
		t.Error("? 应匹配且仅匹配单字节")
	}
	if !wildcardMatch("claude-*", "claude-") {
		t.Error("* 应匹配空串")
	}
}

// TestInboundFallback 外来协议兜底：别名/目录未命中才按档位表换名，显式域前缀保持原域，
// 全不命中落默认模型。别名命中的请求由 chat 入口换名并回显，这里必须原样返回。
func TestInboundFallback(t *testing.T) {
	h := NewHandler(Config{
		Live: livecfg.New(livecfg.Snapshot{
			ModelAliases: livecfg.NormalizeModelAliases([]string{"claude-sonnet-4-5=my-real"}),
		}),
	})
	got := func(model string) string {
		out := h.applyInboundFallback([]byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`))
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("兜底后 body 非法: %v (%s)", err, out)
		}
		return asString(obj["model"])
	}
	cases := map[string]string{
		"claude-sonnet-4-5":      "claude-sonnet-4-5", // 别名命中 → 保持原样
		"claude-sonnet-4":        "glm-5.2",
		"gpt-5-codex":            "kimi-k2.7", // codex 档压过 gpt- 前缀档
		"claude-3-5-haiku":       "glm-5.3-flash",
		"claude-opus-4-1":        "deepseek-v4-pro",
		"global:claude-opus-4-1": "global:deepseek-v4-pro", // 域前缀不丢
		"some-unknown-model":     inboundFallbackModel,
	}
	for in, want := range cases {
		if g := got(in); g != want {
			t.Errorf("applyInboundFallback(%q)=%q want %q", in, g, want)
		}
	}
}

// TestMessagesEndpointInboundFallback 端到端：/v1/messages 发 Claude 模型名时，出站 body
// 的 model 已被换成上游真实名（否则上游 400）。/v1/chat/completions 不做兜底。
func TestMessagesEndpointInboundFallback(t *testing.T) {
	h, sent := aliasTestHandler(t, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(*sent, &out); err != nil {
		t.Fatalf("出站 body 解析失败: %v (%s)", err, *sent)
	}
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2（外来协议档位兜底）", got)
	}

	// 直连 chat 不做兜底：未知模型名原样发出，由上游/既有错误链路处置。
	h2, sent2 := aliasTestHandler(t, nil, nil)
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)))
	var out2 map[string]any
	_ = json.Unmarshal(*sent2, &out2)
	if got := out2["model"]; got != "claude-sonnet-4-5" {
		t.Errorf("chat 直连 model = %v, want 原样 claude-sonnet-4-5", got)
	}
}
