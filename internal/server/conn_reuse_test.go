package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 经网关完整流程的连续请求要复用同一条上游连接（#44）：直连与经代理池两条路都要。
func TestGatewayReusesUpstreamConnection(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "proxied"}[proxied], func(t *testing.T) {
			testGatewayReuse(t, proxied)
		})
	}
}

func testGatewayReuse(t *testing.T, proxied bool) {
	var conns atomic.Int32
	frame := func(delta, finish string) string {
		return `data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":` +
			delta + `,"finish_reason":` + finish + `}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n"
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = io.WriteString(w, frame(`{"content":"hello"}`, "null"))
		f.Flush()
		_, _ = io.WriteString(w, frame(`{}`, `"stop"`))
		f.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
		time.Sleep(20 * time.Millisecond) // 真实上游：chunked 结束块比 [DONE] 晚到十几毫秒，单独一个包
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // 本地自签证书
	up := &upstream.Client{HTTP: &http.Client{Transport: base}, ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	up.ChatHTTP = up.HTTP
	up.IdleTimeout = 300 * time.Second
	if proxied {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			back, err := net.Dial("tcp", r.Host)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			front, rw, _ := w.(http.Hijacker).Hijack()
			_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = rw.Flush()
			go func() { _, _ = io.Copy(back, front); _ = back.Close() }()
			_, _ = io.Copy(front, back)
			_ = front.Close()
		}))
		defer proxy.Close()
		m, err := proxypool.New("", base, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		settings := proxypool.DefaultSettings()
		settings.URL = srv.URL + "/health"
		if err := m.Configure(settings); err != nil {
			t.Fatal(err)
		}
		res, err := m.Import(proxy.URL)
		if err != nil || len(res) != 1 {
			t.Fatal(res, err)
		}
		for range settings.SuccessThreshold {
			if err := m.Probe(context.Background(), res[0].ID); err != nil {
				t.Fatal(err)
			}
		}
		up.ProxyPool = m
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})
	send := func(stream bool) {
		body := `{"model":"glm-5.2","stream":` + map[bool]string{true: "true", false: "false"}[stream] +
			`,"messages":[{"role":"user","content":"hi"}]}`
		ctx, cancel := context.WithCancel(context.Background())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx))
		cancel() // net/http 在处理函数返回后取消请求 context
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	send(true) // 第一个请求建连
	first := conns.Load()
	for range 2 {
		send(true)
	}
	send(false)
	if n := conns.Load() - first; n != 0 {
		t.Fatalf("后 3 个连续请求又新开了 %d 条上游连接，want 0（连接没有回到连接池）", n)
	}
}
