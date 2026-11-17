package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// replayWire 把一个 /v1/responses 请求走完整条出站管线（含 cleanupOrphanToolCalls），
// 返回上游实际收到的 messages。
func replayWire(t *testing.T, body string) []map[string]any {
	t.Helper()
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, `data: {"id":"r1","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
			`data: {"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if len(*reqs) == 0 {
		t.Fatalf("没有发出上游请求，响应: %d %s", rec.Code, rec.Body.String())
	}
	var wire struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte((*reqs)[0]), &wire); err != nil {
		t.Fatalf("出站 body 解析失败: %v", err)
	}
	return wire.Messages
}

func replayRoles(out []any) string {
	var roles []string
	for _, m := range out {
		mm, _ := m.(map[string]any)
		roles = append(roles, asString(mm["role"]))
	}
	return strings.Join(roles, ",")
}

// TestResponsesReplayChatShapedToolKeepsPair Chat 形状的 tool 消息必须带着 tool_call_id
// 出站：丢了它，cleanupOrphanToolCalls 看到的是一条无结果的 tool_call 加一条无主 tool
// 消息，会把这一对调用与结果一起删掉，上游读到的历史里这轮工具调用凭空消失。
func TestResponsesReplayChatShapedToolKeepsPair(t *testing.T) {
	body := `{"model":"glm-5.2","stream":false,` +
		`"tools":[{"type":"function","name":"noop","parameters":{"type":"object","properties":{}}}],"input":[` +
		`{"role":"user","content":"edit"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"ci","type":"function","function":{"name":"noop","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"ci","name":"noop","content":"完成"},` +
		`{"role":"user","content":"继续"}]}`
	msgs := replayWire(t, body)
	var callID, resultID, resultName string
	for _, m := range msgs {
		switch m["role"] {
		case "assistant":
			if tcs, _ := m["tool_calls"].([]any); len(tcs) > 0 {
				callID = asString(tcs[0].(map[string]any)["id"])
			}
		case "tool":
			resultID, resultName = asString(m["tool_call_id"]), asString(m["name"])
		}
	}
	if callID != "ci" || resultID != "ci" || resultName != "noop" {
		t.Fatalf("Chat 形状工具配对被拆：call=%q result=%q name=%q 出站=%v", callID, resultID, resultName, msgs)
	}
}
