package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeContinueUpstream 按调用次数返回脚本化 SSE 并记录每次实际发出的请求体。
// 注意计数口径：单测里「段1」是手工喂给 reader 的（不经 fake），所以 fake 的
// call=1 就是第一次续写请求——script 按「第几次续写」编写。
func fakeContinueUpstream(t *testing.T, script func(call int) (int, string)) (*upstream.Client, *[]string) {
	t.Helper()
	reqs := &[]string{}
	call := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			call++
			raw, _ := io.ReadAll(r.Body)
			*reqs = append(*reqs, string(raw))
			status, body := script(call)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return up, reqs
}

// 段1：正文截断（finish_reason=length），带 usage。
const testSeg1Text = `data: {"id":"cmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}` + "\n\n" +
	`data: {"id":"cmpl-1","choices":[{"index":0,"delta":{"content":"世界"}}]}` + "\n\n" +
	`data: {"id":"cmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
	"data: [DONE]\n\n"

// 续写段：自然收尾（finish_reason=stop），带 usage。
const testSeg2Text = `data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"content":"继续"}}]}` + "\n\n" +
	`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":7,"total_tokens":27}}` + "\n\n" +
	"data: [DONE]\n\n"

func newTestContinueReader(t *testing.T, up *upstream.Client, body string, rc io.ReadCloser) *continueReader {
	t.Helper()
	acct := &auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999}
	return newContinueReader(context.Background(), up, acct, []byte(body), "1.2.3.4", upstream.ChatMeta{}, rc)
}

// TestContinueReaderTextCutContinues 正文截断 → 同模型续写；客户端可见流是一条
// 连续输出：两段正文都在、终态只留 stop、length 不外泄、usage 为跨段合计。
func TestContinueReaderTextCutContinues(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	for _, want := range []string{"你好", "世界", "继续", `"finish_reason":"stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("length 终态不应外泄:\n%s", s)
	}
	// usage：恰好一条，且为跨段合计（10+20 / 5+7 / 15+27）。
	var usageFrames int
	var usage map[string]any
	for _, f := range strings.Split(s, "\n\n") {
		line, ok := strings.CutPrefix(f, "data: ")
		if !ok || line == "" || line == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		if u, ok := obj["usage"].(map[string]any); ok {
			usageFrames++
			usage = u
		}
	}
	if usageFrames != 1 {
		t.Fatalf("usage 帧数=%d want 1:\n%s", usageFrames, s)
	}
	if usage["prompt_tokens"] != float64(30) || usage["completion_tokens"] != float64(12) || usage["total_tokens"] != float64(42) {
		t.Fatalf("usage 合计=%v want prompt=30 completion=12 total=42", usage)
	}
	// 续写请求：同模型 + assistant 携带已输出内容 + 续写指令 + 原始消息仍在。
	req := (*reqs)[0]
	for _, want := range []string{`"model":"glm-5.2"`, "你好世界", "从中断处继续输出剩余内容", `"content":"hi"`} {
		if !strings.Contains(req, want) {
			t.Fatalf("续写请求缺少 %q:\n%s", want, req)
		}
	}
	if !strings.Contains(req, `"role":"assistant"`) {
		t.Fatalf("续写请求缺 assistant 消息:\n%s", req)
	}
}

// TestContinueReaderToolCutNoContinue 工具调用分片在场 → 不续写（零续写请求），
// finish=length 原样外泄（交回既有截断语义）。
func TestContinueReaderToolCutNoContinue(t *testing.T) {
	seg := `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（工具截断不续）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length 终态应原样回放:\n%s", out)
	}
}

// TestContinueReaderExplicitLimitNoContinue 客户端显式输出限额 → 截断是它的
// 预期语义，不续写。
func TestContinueReaderExplicitLimitNoContinue(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newTestContinueReader(t, up, `{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（显式限额不续）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length 终态应原样回放:\n%s", out)
	}
}

// TestContinueReaderSeg2FailureDegrades 续写请求失败 → 降级为截断终态：
// tail 回放（length 在场）、不挂死、账目仍记（段1 usage 合成帧在场）。
func TestContinueReaderSeg2FailureDegrades(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 500, `{"code":11102,"msg":"boom"}`
	})
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1", len(*reqs))
	}
	if !strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("降级后 length 终态应在场:\n%s", s)
	}
	if !strings.Contains(s, `"completion_tokens":5`) {
		t.Fatalf("段1 usage 应合成在场:\n%s", s)
	}
}

// TestContinueReaderSeamReasoningDropped 续写段自带的 reasoning 不外泄（仅累计，
// 供下一段请求带回）：客户端只看到第一段的思考块与连续正文，不产生第二个
// reasoning item（否则 codex 报 "OutputTextDelta without active item"）。
func TestContinueReaderSeamReasoningDropped(t *testing.T) {
	seg1 := `data: {"id":"cmpl-1","choices":[{"index":0,"delta":{"reasoning_content":"公开思考"}}]}` + "\n\n" + testSeg1Text
	seg2 := `data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"reasoning_content":"内部思考"}}]}` + "\n\n" +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"content":"继续"}}]}` + "\n\n" +
		`data: {"id":"cmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, seg2 })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(seg1)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "公开思考") {
		t.Fatalf("段1 思考应外泄:\n%s", s)
	}
	if strings.Contains(s, "内部思考") {
		t.Fatalf("续写段思考不应外泄:\n%s", s)
	}
	if !strings.Contains(s, "继续") {
		t.Fatalf("续写段正文应外泄:\n%s", s)
	}
	// 续写请求应携带第一段累计的思考（上下文一致性；段2 的思考若产生段3 请求才携带）。
	if !strings.Contains((*reqs)[0], "公开思考") {
		t.Fatalf("续写请求应携带累计思考:\n%s", (*reqs)[0])
	}
}

// TestContinueReaderCapAtMaxSegments 连续截断封顶：每段都以 length 结束时，
// 最多 1 次原生 + (maxContinueSegments-1) 次续写，不无限续。
func TestContinueReaderCapAtMaxSegments(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg1Text })
	r := newTestContinueReader(t, up, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		io.NopCloser(strings.NewReader(testSeg1Text)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != maxContinueSegments-1 {
		t.Fatalf("续写请求数=%d want %d（封顶）", len(*reqs), maxContinueSegments-1)
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("封顶后 length 终态应在场:\n%s", out)
	}
}
