// continue_fallthrough_test.go 换段失败落到决策链的下一条。
package server

import (
	"io"
	"strings"
	"testing"
)

// 换段请求失败时落到决策链的下一条，与同步发起时一致：无限额请求的首段只有思考就撞了 length，
// 关思考重发失败后仍应走一次续写（同步版 tryStarvedRetry 返回 false 后就是这么走的）。
func TestContinueSegmentFailureFallsThrough(t *testing.T) {
	seg0 := `data: {"id":"s0","choices":[{"index":0,"delta":{"reasoning_content":"先想一想"}}]}` + "\n\n" +
		`data: {"id":"s0","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		if call == 1 {
			return 500, `{"code":500,"msg":"internal error"}`
		}
		return 200, testSeg2Text
	})
	body := `{"model":"glm-5.2","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(seg0)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("关思考重发失败后应接着续写一次（共 2 个请求），实际 %d 个", len(*reqs))
	}
	if !strings.Contains(string(out), "继续") {
		t.Fatalf("续写段没接上:\n%s", out)
	}
	if n := strings.Count(string(out), `"finish_reason":`); n != 1 {
		t.Fatalf("终态必须恰好一个，实际 %d 个:\n%s", n, out)
	}
}
