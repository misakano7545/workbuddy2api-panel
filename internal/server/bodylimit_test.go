// bodylimit_test.go 入站请求体安全上限（server.max_payload_bytes）。
//
// 三条契约：
//  1. Content-Length 已知且超限 → 立刻 413，不把整段读进来；
//  2. 块上传（无 Content-Length）超限 → 也是 413，不能因为「不知道多长」就读到底；
//  3. 上限之内完全照旧（大请求交上游自然响应，不做预拦截）。
package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestReadBodyOversizeKnownLength Content-Length 声明超限：直接判 413，limit 如实回报。
func TestReadBodyOversizeKnownLength(t *testing.T) {
	h := NewHandler(Config{MaxPayloadBytes: 1 << 10})
	body := strings.Repeat("a", 4<<10)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	got, berr := h.readBody(rec, req)
	if berr == nil || !berr.TooLarge {
		t.Fatalf("4KB body 在 1KB 上限下应判超限, got body=%d berr=%v", len(got), berr)
	}
	if berr.Limit != 1<<10 {
		t.Errorf("limit=%d want 1024", berr.Limit)
	}
}

// TestReadBodyOversizeChunked 没有 Content-Length（块上传）时同样要挡住——并且**不能把
// 整段读进来**：判据是「读了多少字节」，不是「最后有没有报超限」（后者对无界读取也成立）。
func TestReadBodyOversizeChunked(t *testing.T) {
	h := NewHandler(Config{MaxPayloadBytes: 1 << 10})
	cr := &countingReader{r: strings.NewReader(strings.Repeat("a", 64<<10))}
	req := httptest.NewRequest("POST", "/v1/chat/completions", cr)
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	_, berr := h.readBody(rec, req)
	if berr == nil || !berr.TooLarge {
		t.Fatalf("块上传 64KB 在 1KB 上限下应判超限, berr=%v", berr)
	}
	// 上限 1KB：先读 limit+1，排空再读至多 limit，加上缓冲区粒度。
	if cr.n > 2*(1<<10)+4<<10 {
		t.Fatalf("读了 %d 字节，超限请求必须提前停手（不能把 64KB 整段读进内存）", cr.n)
	}
}

// countingReader 数出实际从底层读了多少字节（判「有没有提前停手」用）。
type countingReader struct {
	n int
	r io.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestReadBodyWithinLimit 上限之内照旧读全（多图/长会话不许被网关提前拦）。
func TestReadBodyWithinLimit(t *testing.T) {
	h := NewHandler(Config{MaxPayloadBytes: 64 << 10})
	body := strings.Repeat("a", 32<<10)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	got, berr := h.readBody(rec, req)
	if berr != nil {
		t.Fatalf("32KB body 在 64KB 上限内不该报错: %v", berr)
	}
	if len(got) != len(body) {
		t.Fatalf("read %d bytes want %d", len(got), len(body))
	}
}

// TestChatOversizeBodyIs413 OpenAI 形状入口：413 + payload_too_large，且不打上游。
func TestChatOversizeBodyIs413(t *testing.T) {
	called := false
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		called = true
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:            testPoolWith(&auth.Auth{UID: "b1", AccessToken: "at-b1", ExpiresAt: 9999999999}),
		Upstream:        up,
		MaxPayloadBytes: 1 << 10,
	})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + strings.Repeat("a", 4<<10) + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 413 {
		t.Fatalf("code=%d want 413, body=%s", rec.Code, rec.Body)
	}
	if called {
		t.Error("超限请求不该打到上游（会真花积分）")
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("413 body 不是 OpenAI 错误形状: %v (%s)", err, rec.Body)
	}
	if resp.Error.Code != "payload_too_large" {
		t.Errorf("error.code=%q want payload_too_large", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "max_payload_bytes") {
		t.Errorf("message 应指向配置键（否则运维不知道去哪改）: %q", resp.Error.Message)
	}
}

// TestCountTokensOversizeBodyIsAnthropic 413 在 Anthropic 形状入口要回 Anthropic 错误，
// 否则 Claude Code 那边会当协议错误处理。
func TestCountTokensOversizeBodyIsAnthropic(t *testing.T) {
	h := NewHandler(Config{MaxPayloadBytes: 1 << 10})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + strings.Repeat("a", 4<<10) + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body)))

	if rec.Code != 413 {
		t.Fatalf("code=%d want 413, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("413 body 不是 Anthropic 错误形状: %v (%s)", err, rec.Body)
	}
	if resp.Type != "error" || resp.Error.Type != "invalid_request_error" {
		t.Errorf("形状不对: %+v (%s)", resp, rec.Body)
	}
}
