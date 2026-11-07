package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamInterruptedEmitsTruncationTerminal 上游既没给 finish_reason 也没发
// data: [DONE] 就 EOF（连接说一半断了）时，此前只补 [DONE]：客户端看到「内容 +
// 正常结束」，finish_reason 为空，responses 适配器回 response.completed。Agent 侧
// 的表现就是「说到一半停了、也不报错」（2026-11-07 生产实案：Agent 任务在工具调用
// 之后静默收尾，用户只能手动催）。现在必须补一帧截断终态。
func TestStreamInterruptedEmitsTruncationTerminal(t *testing.T) {
	raw := ": keepalive\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"半句"}}]}` + "\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"`+TruncationFinishReason+`"`) {
		t.Fatalf("断流未补截断终态，body=%s", body)
	}
	// 恰好一个终态：[DONE] 之后不得再出现第二个 finish_reason。
	if n := strings.Count(body, `"finish_reason":"`+TruncationFinishReason+`"`); n != 1 {
		t.Fatalf("截断终态应恰好一帧，got %d", n)
	}
}

// TestStreamNormalDoneNoExtraTerminal 上游正常收尾（给了 [DONE]、且带 finish_reason）
// 时不得多补一帧：两个 finish_reason 会让客户端把后一个当真正的收尾。
func TestStreamNormalDoneNoExtraTerminal(t *testing.T) {
	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"finish_reason":"`+TruncationFinishReason+`"`) {
		t.Fatalf("正常收尾不该补截断终态，body=%s", body)
	}
	if n := strings.Count(body, `"finish_reason":"stop"`); n != 1 {
		t.Fatalf("正常终态应恰好一帧，got %d", n)
	}
}
