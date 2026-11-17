// narration_progress_test.go 第二次救援要求第一次写出了新正文。
package server

import (
	"io"
	"strings"
	"testing"
)

// 救援段一个字都没写出来（只给了 finish=stop）：不发第二次——请求体与第一次一字不差，
// 再发只是再付一次整段上下文的钱。
func TestNarrationNoSecondRescueWithoutProgress(t *testing.T) {
	empty := `data: {"id":"e1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268600,"completion_tokens":3,"total_tokens":268603}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, empty })
	out, err := io.ReadAll(newNarrateReader(t, up, agentBody, segNarrateLeadIn))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("救援段没写出新正文时不该再救，实际 %d 次", len(*reqs))
	}
	if n := strings.Count(string(out), `"finish_reason":`); n != 1 {
		t.Fatalf("终态必须恰好一个，实际 %d 个", n)
	}
}
