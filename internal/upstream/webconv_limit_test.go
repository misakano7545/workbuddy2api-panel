package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 活跃会话上限（10105）必须按业务码判：误判的代价是停掉账号上的活跃会话。
// 只有响应体里真带 code=10105（非 2xx 或 2xx 信封）才算；requestId、URL、文案里碰巧出现
// 10105 的其它失败不算。
func TestIsWebConversationLimitByCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"429 + code 10105", 429, `{"code":10105,"msg":"maximum number of active conversations"}`, true},
		{"200 envelope code 10105", 200, `{"code":10105,"msg":"limit"}`, true},
		{"500 requestId contains 10105", 500, `{"code":10001,"msg":"boom","requestId":"r-10105-x"}`, false},
		{"429 other code, msg mentions 10105", 429, `{"code":6004,"msg":"see 10105"}`, false},
		{"non-JSON body mentions 10105", 502, `bad gateway 10105`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
			_, err := c.CreateWebConversation(context.Background(), webTestAuth(), "Hi", "m")
			if err == nil {
				t.Fatal("want error")
			}
			if got := IsWebConversationLimit(err); got != tc.want {
				t.Fatalf("IsWebConversationLimit(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
	if IsWebConversationLimit(nil) {
		t.Fatal("nil error is not a limit")
	}
}
