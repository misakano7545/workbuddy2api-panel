// continue_close_test.go 换段请求在途时 Close：取消请求、收走迟到的响应体。
package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// trackedBody 记录自己有没有被关掉。
type trackedBody struct {
	io.Reader
	closed chan struct{}
}

func (b *trackedBody) Close() error { close(b.closed); return nil }

// 换段请求在途时客户端断开（空闲超时、写保活失败，StreamHint 随之返回，handler 的 defer
// 调 Close）：在途请求要被取消，迟到的响应体要被关掉。否则发起协程一直卡在上游，直到请求
// context 结束；响应若恰好在这时到达，body 连同连接一直没人关。
func TestContinueCloseReclaimsPendingSegment(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 10 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })

	cancelled := make(chan struct{})
	late := &trackedBody{Reader: strings.NewReader(testSeg2Text), closed: make(chan struct{})}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done() // 上游迟迟不回，直到请求被取消
			close(cancelled)
			// 响应恰好在取消的同时到达：仍交回一个 body，由 Close 负责收走。
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: late}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	r := newTestContinueReader(t, up, testContinueBody, io.NopCloser(strings.NewReader(testSeg1Text)))
	buf := make([]byte, 64*1024)
	deadline := time.Now().Add(3 * time.Second)
	for {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatalf("续写请求在途时流不该结束: %v", err)
		}
		if strings.Contains(string(buf[:n]), ": keepalive") {
			break // 换段请求在途，客户端在这里断开
		}
		if time.Now().After(deadline) {
			t.Fatal("等不到换段请求在途的保活")
		}
	}
	r.Close()
	for _, c := range []struct {
		name string
		ch   chan struct{}
	}{{"在途请求被取消", cancelled}, {"迟到的响应体被关掉", late.closed}} {
		select {
		case <-c.ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("Close 之后%s没有发生", c.name)
		}
	}
}
