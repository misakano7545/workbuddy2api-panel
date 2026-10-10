package upstream

import (
	"strings"
	"testing"
)

// TestAggregateMarksCutAsTruncated 非流式（Aggregate）路径的同一根因：上游既没给
// finish_reason 也没给 data: [DONE] 就 EOF（断流）时，此前下发的是初始默认值
// "stop"——等于告诉客户端「模型正常说完了」，半截正文被当完整结果。判据与同文件
// 处理残缺 tool_calls 的 !sawDone 保持一致。
func TestAggregateMarksCutAsTruncated(t *testing.T) {
	const content = `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"半截正文"}}]}` + "\n\n"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"断流（无 finish、无 [DONE]）", content, TruncationFinishReason},
		{"正常收尾 [DONE]", content + "data: [DONE]\n\n", "stop"},
		{"显式 finish_reason（无 [DONE]）",
			content + `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n", "stop"},
		{"显式 length（无 [DONE]）",
			content + `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n", "length"},
	}
	for _, tc := range cases {
		resp, err := Aggregate(strings.NewReader(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		choices, _ := resp["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("%s: choices=%v", tc.name, resp["choices"])
		}
		c0, _ := choices[0].(map[string]any)
		if got := c0["finish_reason"]; got != tc.want {
			t.Errorf("%s: finish_reason=%v want %v", tc.name, got, tc.want)
		}
	}
}

// TestAggregateEmptyStreamStillErrors 空流（0 有效帧）必须仍是错误，不能被截断终态
// 伪装成「有内容的 200」——客户端与运维都靠它区分「上游空转」与「流被切」。
func TestAggregateEmptyStreamStillErrors(t *testing.T) {
	if _, err := Aggregate(strings.NewReader("data: [DONE]\n\n")); err == nil {
		t.Fatal("仅 [DONE] 的空流应报 errEmptyStream，不得合成截断响应")
	}
}
