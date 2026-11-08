// 上游 reasoning parser 偶发把整段 CoT 当正文下发（issue #194）：usage 里
// reasoning_tokens=0，content 首尾带模型自己的控制 token <think:SUF>。Anthropic
// 客户端只认 content_block.type=thinking，内联标签一律当正文显示。下面钉住
// messages 侧两条路径都不把这些 token 发成 text_delta。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagesStreamSplitsInlineThinkTag(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &messagesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		// 标签跨片：第一片只到 "<thi"。
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"<thi"}}]}` + "\n\n",
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"nk:6124c78e>CoT-A</think:6124c78e>Ans"}}]}` + "\n\n",
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"wer"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, f := range frames {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if strings.Contains(body, "6124c78e") || strings.Contains(body, "<think") || strings.Contains(body, "</think") {
		t.Fatalf("控制 token 泄漏到客户端: %s", body)
	}
	if !strings.Contains(body, `"thinking":"CoT-A"`) {
		t.Fatalf("CoT 应作为 thinking_delta: %s", body)
	}
	if !strings.Contains(body, `"text":"Ans"`) || !strings.Contains(body, `"text":"wer"`) {
		t.Fatalf("正文应完整: %s", body)
	}
	if ti, xi := strings.Index(body, "thinking_delta"), strings.Index(body, `"type":"text_delta"`); ti < 0 || xi < 0 || ti > xi {
		t.Fatalf("thinking 应先于文本: ti=%d xi=%d\n%s", ti, xi, body)
	}
}

// 上游若用非 delta 的整条 message 帧，此前整帧被丢（正文与思考一起消失）。
func TestMessagesStreamAcceptsFullMessageFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &messagesWriter{ResponseWriter: rec, stream: true}
	rw.Header().Set("Content-Type", "text/event-stream")
	for _, f := range []string{
		`data: {"id":"c","choices":[{"index":0,"message":{"reasoning_content":"R","content":"C"}}]}` + "\n\n",
		// message 已整条给过，后续 delta 不再重复拼。
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"dup"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		if _, err := rw.Write([]byte(f)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()
	body := rec.Body.String()
	if !strings.Contains(body, `"thinking":"R"`) || !strings.Contains(body, `"text":"C"`) {
		t.Fatalf("完整 message 未转出: %s", body)
	}
	if strings.Contains(body, "dup") {
		t.Fatalf("整条 message 之后又拼了 delta: %s", body)
	}
}

func TestChatCompletionToMessageSplitsInlineThinkTag(t *testing.T) {
	obj := map[string]any{
		"id":    "chatcmpl-1",
		"model": "hy4-preview",
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": "<think:6124c78e>CoT-A</think:6124c78e>Answer",
			},
		}},
	}
	msg := chatCompletionToMessage(obj)
	raw, _ := json.Marshal(msg["content"])
	got := string(raw)
	if strings.Contains(got, "6124c78e") || strings.Contains(got, "<think") {
		t.Fatalf("控制 token 泄漏: %s", got)
	}
	if !strings.Contains(got, `{"thinking":"CoT-A","type":"thinking"}`) {
		t.Fatalf("CoT 应成 thinking 块: %s", got)
	}
	if !strings.Contains(got, `{"text":"Answer","type":"text"}`) {
		t.Fatalf("正文应完整: %s", got)
	}
}

// 正文中间提到这几个字（不是开头）时不得改写。
func TestChatCompletionToMessageKeepsMidTextThinkMention(t *testing.T) {
	obj := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "说明里提到 <think:abc> 这种 token"},
		}},
	}
	blocks, _ := chatCompletionToMessage(obj)["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "text" ||
		blocks[0].(map[string]any)["text"] != "说明里提到 <think:abc> 这种 token" {
		t.Fatalf("正文被误改: %v", blocks)
	}
}
