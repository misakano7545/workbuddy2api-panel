package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestFetchEndpoint POST /v1/fetch：DSH 的 DeepSeek Fetch API 形状——
// 请求 {url}，回 {content}（上游原文的 title/url 一并保留）。
func TestFetchEndpoint(t *testing.T) {
	const upstreamBody = `{"url":"https://example.com/a","title":"示例","content":"# 标题\n\n正文"}`
	f := &searchFake{status: 200, body: upstreamBody}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://example.com/a"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if got["content"] != "# 标题\n\n正文" {
		t.Fatalf("content 透传不符: %v", got["content"])
	}
	if got["title"] != "示例" || got["url"] != "https://example.com/a" {
		t.Fatalf("其余字段应原样保留: %v", got)
	}
	if f.paths[0] != "/agenttool/v1/webfetch" {
		t.Fatalf("落点应为 webfetch: %v", f.paths)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	if sent["url"] != "https://example.com/a" {
		t.Fatalf("出站 url 不符: %v", sent)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type=%q", ct)
	}
}

// TestFetchValidation 本地先拦：缺 url / 非 http(s) 一律 400 且不打上游。
func TestFetchValidation(t *testing.T) {
	f := &searchFake{status: 200, body: `{"content":"x"}`}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	for _, body := range []string{
		`{}`,
		`{"url":"  "}`,
		`{"url":"file:///etc/passwd"}`,
		`{"url":"not-a-url"}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s code=%d, want 400 (%s)", body, rec.Code, rec.Body)
		}
	}
	if len(f.paths) != 0 {
		t.Fatalf("校验失败不应打上游，实际 %d 次", len(f.paths))
	}
}

// TestFetchUpstreamShape 上游 200 但没有 string content → 502，不伪造空正文。
func TestFetchUpstreamShape(t *testing.T) {
	for _, body := range []string{`{"url":"https://x"}`, `{"content":null}`, `{"content":123}`, `not json`} {
		f := &searchFake{status: 200, body: body}
		p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
		h := NewHandler(Config{Pool: p, Upstream: f.client()})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
			strings.NewReader(`{"url":"https://x"}`)))
		if rec.Code != http.StatusBadGateway {
			t.Errorf("upstream=%s code=%d, want 502 (%s)", body, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), `"content":""`) {
			t.Errorf("不应把坏响应伪造成空正文: %s", rec.Body)
		}
	}
}

// TestFetchRequestLevelFailFast 上游 400（url 校验）→ 原文透传、只打一次、不轮转。
func TestFetchRequestLevelFailFast(t *testing.T) {
	f := &searchFake{status: 400, body: `{"code":15013,"msg":"url is required"}`}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://x"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "15013") {
		t.Fatalf("请求级 400 应原文透传，得到 %d %s", rec.Code, rec.Body.String())
	}
	if len(f.paths) != 1 {
		t.Fatalf("请求级错误应 fail-fast 不轮转，实际打了 %d 次", len(f.paths))
	}
}

// TestFetchFallsBackToGlobalOnDomainRejected 2026-10-31 实案：CN 域 edgebrowser 的
// 域名路由策略拒绝 Google 系域名（403 code=15021），global 域同一域名正常。
// 换遍全池 CN 号都是同一个 403（域名级），只有换域有救 → /v1/fetch 必须自动改域。
func TestFetchFallsBackToGlobalOnDomainRejected(t *testing.T) {
	var hosts []string
	f := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)
			status, body := http.StatusForbidden,
				`{"code":15021,"msg":"edgebrowser: API error: domain rejected by via routing policy"}`
			if r.URL.Host == "global.example" {
				status, body = http.StatusOK,
					`{"url":"https://android.googlesource.com/x","title":"t","content":"# ok"}`
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		GlobalEnabled:  true,
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://android.googlesource.com/x"}`)))
	if rec.Code != 200 {
		t.Fatalf("应跨域成功，得到 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"# ok"`) {
		t.Fatalf("content 不符: %s", rec.Body.String())
	}
	if len(hosts) != 2 || hosts[0] != "cn.example" || hosts[1] != "global.example" {
		t.Fatalf("应先 CN 后 global，实际 hosts=%v", hosts)
	}
}

// TestFetchNoCrossRealmOnOtherErrors 只有「域名被策略拒绝」才跨域：
// 5xx / 普通 4xx 跨域重试毫无意义（两域积分、账号互不相通），不该白烧 global 号。
func TestFetchNoCrossRealmOnOtherErrors(t *testing.T) {
	var hosts []string
	f := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":10000,"msg":"boom"}`)),
			}, nil
		})},
		GlobalEnabled:  true,
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://example.com/a"}`)))
	if rec.Code == 200 {
		t.Fatalf("5xx 不该成功: %s", rec.Body.String())
	}
	for _, host := range hosts {
		if host != "cn.example" {
			t.Fatalf("5xx 不该跨域重试，实际 hosts=%v", hosts)
		}
	}
}

// TestFetchDownloadHint 下载类 URL（gitiles ?format=TEXT）连 global 域都救不了：
// 上游浏览器回 422「Browser does not support download functionality」。此时应把
// 提示改成可操作的建议（换 HTML 页面地址），而不是把上游内部文案原样甩给用户。
func TestFetchDownloadHint(t *testing.T) {
	f := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body := http.StatusForbidden,
				`{"code":15021,"msg":"edgebrowser: API error: domain rejected by via routing policy"}`
			if r.URL.Host == "global.example" {
				status, body = http.StatusInternalServerError,
					`{"code":10000,"msg":"search post: unexpected status 422: Browser does not support download functionality"}`
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		GlobalEnabled:  true,
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://android.googlesource.com/x?format=TEXT"}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "HTML") || !strings.Contains(rec.Body.String(), "format=TEXT") {
		t.Fatalf("应给出换 HTML 地址的可操作提示: %s", rec.Body.String())
	}
}

// TestFetchDomainRejectedDoesNotPenalizeAccounts 15021 是域名级策略拒绝（同域所有
// 账号同一结果），不是账号问题。若按 ErrClient 喂连败，连抓几个 Google 文档就会把
// 整个 CN 池逐个降到出池——这里连打 6 轮（> degradeThreshold=5）断言账号零惩罚。
func TestFetchDomainRejectedDoesNotPenalizeAccounts(t *testing.T) {
	f := &searchFake{status: http.StatusForbidden,
		body: `{"code":15021,"msg":"edgebrowser: API error: domain rejected by via routing policy"}`}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	for i := 1; i <= 6; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
			strings.NewReader(`{"url":"https://www.google.com/"}`)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("run %d: code=%d want 403 (%s)", i, rec.Code, rec.Body)
		}
	}
	for _, uid := range []string{"c1", "c2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ConsecutiveFails != 0 || !st.DegradeUntil.IsZero() {
			t.Errorf("%s: 15021 域名级拒绝不得罚账号: %+v", uid, st)
		}
	}
}
