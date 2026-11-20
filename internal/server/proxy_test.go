package server

import (
	"context"
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

func TestProxyFailureDoesNotPenalizeAccount(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "refresh"}[refresh], func(t *testing.T) {
			up := upstream.New()
			manager, err := proxypool.New("", up.HTTP.Transport.(*http.Transport), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			// Pending proxies are never contacted and must not silently fall back to direct.
			if _, err := manager.Import("http://localhost:8080"); err != nil {
				t.Fatal(err)
			}
			up.ProxyPool = manager
			a := &auth.Auth{UID: "u1", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: 9999999999}
			if refresh {
				a.ExpiresAt = 1
			}
			p := testPoolWith(a)
			h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1, RefreshSkew: time.Minute})
			for range 4 {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)))
				if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "proxy_unavailable") {
					t.Fatalf("response=%d %s", rec.Code, rec.Body.String())
				}
			}
			s, _ := p.Status("u1")
			if s.Disabled || s.Cooling || s.ConsecutiveFails != 0 || s.BreakerFails != 0 || !s.DegradeUntil.IsZero() {
				t.Fatalf("proxy failure penalized account: %+v", s)
			}
		})
	}
}

func TestProxyFailureDoesNotReplayAccountRequests(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN = "http://upstream.invalid", "http://upstream.invalid"
	manager, err := proxypool.New("", up.HTTP.Transport.(*http.Transport), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	settings := proxypool.DefaultSettings()
	settings.URL = "http://health.invalid/health"
	if err := manager.Configure(settings); err != nil {
		t.Fatal(err)
	}
	added, err := manager.Import(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Probe(context.Background(), added[0].ID); err != nil {
		t.Fatal(err)
	}
	up.ProxyPool = manager
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "access", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "access", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3, EmbeddingsEnabled: true})
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/search", `{"query":"hi"}`},
		{"/v1/tools/search", `{"query":"hi"}`},
		{"/v1/images/generations", `{"model":"cn:hunyuan-image-alpha","prompt":"hi"}`},
		{"/v1/embeddings", `{"model":"bge-m3","input":"hi"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			calls.Store(0)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "proxy_unavailable") || calls.Load() != 1 {
				t.Fatalf("response=%d %s; proxy calls=%d", rec.Code, rec.Body.String(), calls.Load())
			}
		})
	}
}
