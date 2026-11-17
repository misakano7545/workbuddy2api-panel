// narration_nudge_test.go 结构未闭合的尾部用预告句指令。
package server

import (
	"encoding/json"
	"io"
	"testing"
)

// 截断的调用标记、未闭合代码块也没有标点，但不是承诺句：仍用「停在预告上、把剩下的写完」那条指令。
func TestNarrationStructurallyOpenTailKeepsLeadInNudge(t *testing.T) {
	for _, text := range []string{
		"我先跑一下：\n<｜｜DSML｜｜ invoke name=\"bash\">\n<｜｜DSML｜｜ parameter name=\"cmd\">go test ./... 2>&1</｜｜DSML｜｜ parameter>",
		"先写一个最小复现：\n```go\nfunc main() {",
	} {
		raw, _ := json.Marshal(text)
		seg := `data: {"id":"o1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
			`data: {"id":"o1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":80,"total_tokens":268618}}` + "\n\n" +
			"data: [DONE]\n\n"
		up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
		if _, err := io.ReadAll(newNarrateReader(t, up, agentBody, seg)); err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(*reqs) != 1 {
			t.Fatalf("结构未闭合应救一次（%q），实际 %d 次", text, len(*reqs))
		}
		msgs, _ := reqBody(t, (*reqs)[0])["messages"].([]any)
		last, _ := msgs[len(msgs)-1].(map[string]any)
		if got, _ := last["content"].(string); got != narrationNudge {
			t.Errorf("结构未闭合（%q）应带 narrationNudge，实际 %q", text, got)
		}
	}
}
