package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// sseReasoningOnly 上游把整轮预算花在思考上：只有 reasoning_content、正文零字节、
// 没有 tool_calls，收尾 finish_reason=stop（不是 length）。这是 cn:deepseek 系在
// effort=max 下的高频形态（2026-11-07 本机实测 think/completion 可到 0.93）。
const sseReasoningOnly = `data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"先想清楚需求"}}]}` + "\n\n" +
	`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"reasoning_content":"再权衡方案"}}]}` + "\n\n" +
	`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":200,"total_tokens":210,"completion_thinking_tokens":200}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestResponsesStreamReasoningOnlyFails 只有思考、没有正文与工具调用时，流式必须发
// response.failed 而不是 response.completed：completed 等于告诉客户端「模型想完了、
// 没话要说」，Agent 就此收尾、用户看到空回复（issue #508 同判据，借鉴 hub）。
func TestResponsesStreamReasoningOnlyFails(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseReasoningOnly, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	h.ServeHTTP(rec, req)

	types := sseTypes(rec.Body.String())
	if containsStr(types, "response.completed") {
		t.Fatalf("只有思考不应回 completed：types=%v\n%s", types, rec.Body.String())
	}
	if !containsStr(types, "response.failed") {
		t.Fatalf("应回 response.failed：types=%v\n%s", types, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "empty_reasoning_only") {
		t.Fatalf("失败原因应可辨（empty_reasoning_only）：\n%s", rec.Body.String())
	}
}

// TestResponsesNonStreamReasoningOnly502 非流式同一条判据：502 + empty_reasoning_only，
// 让客户端按自己的重试策略重来，而不是拿到一个「成功但 output 全空」的对象。
func TestResponsesNonStreamReasoningOnly502(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseReasoningOnly, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body.Bytes())
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("body 非 JSON: %s", rec.Body.Bytes())
	}
	e, _ := obj["error"].(map[string]any)
	if e == nil || e["code"] != "empty_reasoning_only" {
		t.Fatalf("error=%v", obj["error"])
	}
}

// TestResponsesReasoningOnlyNotTriggeredByLength length 截断（正文为空但有额度原因）
// 必须仍然走 incomplete：它是「额度用尽」而不是「上游故障」，变成 failed 会让客户端
// 误以为是链路问题去重试，白烧额度。
func TestResponsesReasoningOnlyNotTriggeredByLength(t *testing.T) {
	const sseLen = `data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"reasoning_content":"想想想"}}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseLen, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	h.ServeHTTP(rec, req)
	types := sseTypes(rec.Body.String())
	if !containsStr(types, "response.incomplete") {
		t.Fatalf("length 应走 incomplete：types=%v", types)
	}
	if containsStr(types, "response.failed") {
		t.Fatalf("length 不该被抢成 failed：types=%v", types)
	}
}

// TestResponsesReasoningWithToolCallNotFailed 思考 + 工具调用是正常回合（模型想完就
// 动手），不能在收尾时被判成「只有思考」。
func TestResponsesReasoningWithToolCallNotFailed(t *testing.T) {
	const sseTool = `data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"reasoning_content":"需要执行命令"}}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseTool, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"glm-5.2","input":"hi","stream":true,"tools":[{"type":"function","name":"Bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]}`))
	h.ServeHTTP(rec, req)
	types := sseTypes(rec.Body.String())
	if containsStr(types, "response.failed") {
		t.Fatalf("思考+工具调用不该判 failed：types=%v\n%s", types, rec.Body.String())
	}
	if !containsStr(types, "response.completed") {
		t.Fatalf("应 completed：types=%v", types)
	}
}
