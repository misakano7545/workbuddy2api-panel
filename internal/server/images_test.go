package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// imageFake 记录图像请求落到哪个 base、body 里的 model，并按脚本回信封。
type imageFake struct {
	paths  []string
	hosts  []string
	bodies []string
	status int
	body   string
}

func (f *imageFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			f.paths = append(f.paths, r.URL.Path)
			f.hosts = append(f.hosts, r.URL.Host)
			f.bodies = append(f.bodies, string(raw))
			return &http.Response{
				StatusCode: f.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(f.body)),
			}, nil
		})},
		ChatBaseCN:     "https://cn.example",
		BillingBaseCN:  "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
}

const imageOK = `{"code":0,"msg":"OK","data":{"created":1791104365,"data":[{"url":"https://cos.example/img.png?sig=x"}],` +
	`"usage":{"input_tokens":336,"output_tokens":518,"total_tokens":854,"credit":5.71}}}`

func imagePool() *pool.Pool {
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	return p
}

// TestImagesGenerations 文生图端点：模型前缀定 realm（前缀不落上游）、信封原样透出、
// 上游错误按其状态码与原文透出、缺 prompt 400。
func TestImagesGenerations(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)))
		return rec
	}

	// ① 国内：cn: 前缀剥掉进上游，回程 data 原样（含 usage/credit）
	rec := post(`{"model":"cn:hunyuan-image-alpha","prompt":"一只猫","n":1,"size":"1024x1024"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.paths[0] != "/v2/images/generations" || f.hosts[0] != "cn.example" {
		t.Fatalf("落点 %s%s want /v2/images/generations @ cn.example", f.hosts[0], f.paths[0])
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	if sent["model"] != "hunyuan-image-alpha" {
		t.Fatalf("上游收到的 model=%v want 裸名（前缀是网关路由协议）", sent["model"])
	}
	if sent["size"] != "1024x1024" {
		t.Fatalf("其余字段应原样透传，size=%v", sent["size"])
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("回程不是 JSON：%s", rec.Body.String())
	}
	items, _ := got["data"].([]any)
	if len(items) != 1 || !strings.Contains(rec.Body.String(), "https://cos.example/img.png") {
		t.Fatalf("回程应原样带出 url：%s", rec.Body.String())
	}
	if u, _ := got["usage"].(map[string]any); u == nil || u["credit"] != 5.71 {
		t.Fatalf("回程应保留上游 usage/credit：%v", got["usage"])
	}

	// ② 国际：global: 前缀 → 走 global base（混元在国际版无路由，必须能落到对的池）
	f.paths, f.hosts, f.bodies = nil, nil, nil
	if rec := post(`{"model":"global:gpt-image-2.5-sunburst","prompt":"a cat"}`); rec.Code != 200 {
		t.Fatalf("global 出图 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.hosts[0] != "global.example" {
		t.Fatalf("global 应走 ChatBaseGlobal，实际 %s", f.hosts[0])
	}

	// ③ 上游信封错误（国际版拿混元调 = 实测 14401）：状态码与原文都透出
	f.status, f.body = 400, `{"code":14401,"msg":"Create image failed with error: Image model [hunyuan-image-alpha] route config not found"}`
	rec = post(`{"model":"global:hunyuan-image-alpha","prompt":"a cat"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "14401") {
		t.Fatalf("上游错误应透出其状态码与原文：code=%d body=%s", rec.Code, rec.Body.String())
	}

	// ④ 缺 prompt → 400；缺 model 用缺省 cn 图像模型
	if rec := post(`{"model":"cn:hunyuan-image-alpha"}`); rec.Code != 400 {
		t.Fatalf("缺 prompt 应 400，实际 %d", rec.Code)
	}
	f.status, f.body = 200, imageOK
	f.paths, f.bodies = nil, nil
	if rec := post(`{"prompt":"一只猫"}`); rec.Code != 200 {
		t.Fatalf("缺省模型应可用，实际 %d %s", rec.Code, rec.Body.String())
	}
	var def map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &def)
	if def["model"] != "hunyuan-image-alpha" {
		t.Fatalf("缺省模型=%v want hunyuan-image-alpha", def["model"])
	}
}

// TestImagesGenerationsNoAccountForRealm 该 realm 没有可用账号时明确报错（而不是回落到另一域）。
func TestImagesGenerationsNoAccountForRealm(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"global:gpt-image-2.5-sunburst","prompt":"a cat"}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	if len(f.paths) != 0 {
		t.Fatalf("不该发起上游调用，实际落到 %v", f.paths)
	}
}
