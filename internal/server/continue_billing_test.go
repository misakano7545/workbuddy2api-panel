package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// scriptedChatHandler 造一个单账号 handler，上游第 n 次调用返回 segs[n-1]（超出时重复最后一段）。
func scriptedChatHandler(t *testing.T, segs ...string) (*Handler, *[]string) {
	t.Helper()
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		if call > len(segs) {
			call = len(segs)
		}
		return 200, segs[call-1]
	})
	up.StoreModelMaxOutForTest("cn", map[string]int64{"deepseek-v4.1-flash": 131072})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: p, Upstream: up}), reqs
}

func bookedTokens(t *testing.T, h *Handler) (prompt, completion, total int64) {
	t.Helper()
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	return st.TokenUsage.PromptTokens, st.TokenUsage.CompletionTokens, st.TokenUsage.TotalTokens
}

// failAfterWriter 模拟客户端中途断开：写到含 marker 的字节时取消请求 context、之后写全部报错。
type failAfterWriter struct {
	*httptest.ResponseRecorder
	marker string
	cancel context.CancelFunc
	broken bool
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.broken || strings.Contains(string(p), w.marker) {
		w.broken = true
		w.cancel()
		return 0, errors.New("client gone")
	}
	return w.ResponseRecorder.Write(p)
}

// WriteString 必须一并覆盖：ResponseRecorder 自带 WriteString，io.WriteString 会绕过 Write。
func (w *failAfterWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

// ctxBlockReader 一直阻塞到 ctx 取消，然后以 ctx 的错误结束。
type ctxBlockReader struct{ ctx context.Context }

func (r ctxBlockReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// TestContinuationBillingSumsEverySegment 续写与救援的每一段都是一次单独计费的上游调用，
// 记到账号上的用量必须是各段之和（prompt 也是每段各算一次）。客户端看到的合计 usage
// 只留末段 prompt（不虚报上下文占用），那是给客户端看的，不是账。
//
// 三种形态都漏过账：
//   - 流式续写：只按客户端可见的合计记，前段 prompt 丢了；
//   - 非流式空截断救援：resp 被救援结果整个替换，首次调用的用量丢了；
//   - 客户端在续写段中途断开：合计 usage 帧根本没发出来，已结束各段一笔都没记。
func TestContinuationBillingSumsEverySegment(t *testing.T) {
	// 续写段写出正文后连接被客户端掐断：上游这段既没有 finish 也没有 usage。
	seg2Cut := `data: {"id":"cmpl-2","choices":[{"index":0,"delta":{"content":"继续"}}]}` + "\n\n"
	// 同一段里 usage 出现在多帧（有的上游每帧都带）：段内按客户端可见合计的同一规则合并
	// （prompt 取最新、输出求和），只有一段时记的与客户端看到的一致，不因记账改造翻倍 prompt。
	multiUsageSeg := `data: {"id":"m","choices":[{"index":0,"delta":{"content":"一"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\n" +
		`data: {"id":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}` + "\n\n" +
		"data: [DONE]\n\n"
	cases := []struct {
		name   string
		body   string
		segs   []string
		abort  bool
		prompt int64
		compl  int64
		total  int64
	}{
		{
			name:   "流式续写两段",
			body:   `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			segs:   []string{testSeg1Text, testSeg2Text},
			prompt: 10 + 20, compl: 5 + 7, total: 15 + 27,
		},
		{
			name:   "单段多帧 usage",
			body:   `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			segs:   []string{multiUsageSeg},
			prompt: 10, compl: 5, total: 15,
		},
		{
			name:   "非流式空截断救援",
			body:   `{"model":"deepseek-v4.1-flash","max_tokens":1024,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			segs:   []string{segStarvedAllThinking, testSeg2Text},
			prompt: 10 + 20, compl: 8192 + 7, total: 8202 + 27,
		},
		{
			name:  "客户端在续写段中途断开",
			body:  `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			segs:  []string{testSeg1Text, seg2Cut},
			abort: true,
			// 首段完整结束（usage 已到），续写段没有 usage：能记的就是首段。
			prompt: 10, compl: 5, total: 15,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, reqs := scriptedChatHandler(t, c.segs...)
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(c.body))
			var w http.ResponseWriter = httptest.NewRecorder()
			if c.abort {
				// 续写段吐完一帧后挂住，直到客户端断开（请求 context 取消）才随之断流——与真实
				// 连接一致：上游这一段还没写完，人已经走了。
				h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					*reqs = append(*reqs, "")
					body := io.Reader(strings.NewReader(c.segs[0]))
					if len(*reqs) > 1 {
						body = io.MultiReader(strings.NewReader(c.segs[1]), ctxBlockReader{r.Context()})
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: io.NopCloser(body)}, nil
				})
				ctx, cancel := context.WithCancel(req.Context())
				defer cancel()
				req = req.WithContext(ctx)
				w = &failAfterWriter{ResponseRecorder: httptest.NewRecorder(), marker: "继续", cancel: cancel}
			}
			h.ServeHTTP(w, req)
			if len(*reqs) != len(c.segs) {
				t.Fatalf("上游调用=%d want %d", len(*reqs), len(c.segs))
			}
			p, cpl, tot := bookedTokens(t, h)
			if p != c.prompt || cpl != c.compl || tot != c.total {
				t.Fatalf("记账 prompt/completion/total=%d/%d/%d want %d/%d/%d", p, cpl, tot, c.prompt, c.compl, c.total)
			}
		})
	}
}

// TestHeadVetoAfterRescueBooksRescuedSegment 首段零输出（思考没外泄、正文 0B、finish=length）
// 触发关思考重发，救援段第一帧就是 error 帧 → 首帧否决换号。客户端一个字节都没收到，
// 但首段已经是一次计费的上游调用，不能记成零。
func TestHeadVetoAfterRescueBooksRescuedSegment(t *testing.T) {
	silentStarved := `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":1024,"completion_thinking_tokens":1024,"total_tokens":1034}}` + "\n\n" +
		"data: [DONE]\n\n"
	errFrame := `data: {"error":{"message":"boom","code":"internal_error"}}` + "\n\n" + "data: [DONE]\n\n"
	h, reqs := scriptedChatHandler(t, silentStarved, errFrame)
	body := `{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":1024,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if len(*reqs) < 2 {
		t.Fatalf("上游调用=%d want ≥2（首段 + 救援段）", len(*reqs))
	}
	p, cpl, tot := bookedTokens(t, h)
	if p != 10 || cpl != 1024 || tot != 1034 {
		t.Fatalf("首帧否决前已跑完的首段应记账，got prompt/completion/total=%d/%d/%d want 10/1024/1034", p, cpl, tot)
	}
}
