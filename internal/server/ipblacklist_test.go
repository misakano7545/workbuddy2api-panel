package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// issue #244：入站 IP 黑名单必须在入口（鉴权与请求日志之前）拦掉，且判定用的
// 客户端 IP 不能被伪造的 X-Forwarded-For 绕过，也不能因此把面板/探活关在门外。
func TestIPBlacklistGatesIngress(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		path       string
		wantCode   int
	}{
		{"命中单 IP", "104.233.189.37:5000", "", "/v1/chat/completions", http.StatusForbidden},
		{"命中 CIDR", "45.12.3.4:5000", "", "/v1/chat/completions", http.StatusForbidden},
		{"未命中放行到鉴权", "8.8.4.4:5000", "", "/v1/chat/completions", http.StatusUnauthorized},
		// 直连对端是公网地址：XFF 由对端自己填，必须无视——否则拉黑形同虚设。
		{"公网对端伪造 XFF 绕不过", "8.8.4.4:5000", "1.1.1.1", "/v1/chat/completions", http.StatusUnauthorized},
		{"公网对端伪造 XFF 栽赃无效", "203.0.113.9:5000", "104.233.189.37", "/v1/chat/completions", http.StatusUnauthorized},
		// 反代同机（Caddy 127.0.0.1）：XFF 才是真实客户端，按它判。
		{"回环对端采信 XFF", "127.0.0.1:5000", "104.233.189.37", "/v1/chat/completions", http.StatusForbidden},
		{"回环对端 XFF 未命中", "127.0.0.1:5000", "9.9.9.9", "/v1/chat/completions", http.StatusUnauthorized},
		// 面板是唯一的在线恢复入口；探活失败会触发重启循环。两者都豁免。
		// 本用例未挂面板 handler，故走 mux 的 404——重点是「不是 403」。
		{"面板豁免", "104.233.189.37:5000", "", "/panel/api/overview", http.StatusNotFound},
		{"探活豁免", "104.233.189.37:5000", "", "/healthz", http.StatusOK},
	}
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		// 设了 key 但请求不带 Authorization：放行后的期望码是 401，
		// 既能验证「不是被黑名单拦的」，也不会真的打到上游。
		APIKey:      "k",
		IPBlacklist: []string{"104.233.189.37", "45.12.0.0/16"},
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", c.path, strings.NewReader(`{}`))
			if c.path == "/healthz" {
				req = httptest.NewRequest("GET", c.path, nil)
			}
			req.RemoteAddr = c.remoteAddr
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantCode {
				t.Fatalf("code=%d want=%d body=%s", rec.Code, c.wantCode, rec.Body)
			}
			if c.wantCode == http.StatusForbidden && !strings.Contains(rec.Body.String(), "ip_blocked") {
				t.Fatalf("403 必须带 ip_blocked 错误码，body=%s", rec.Body)
			}
		})
	}
}
