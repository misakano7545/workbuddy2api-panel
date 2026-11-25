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

// embedFake 记录 /v2/embeddings 的落点与请求体，按脚本回状态与原文。
type embedFake struct {
	paths  []string
	bodies []string
	status int
	body   string
}

func (f *embedFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			f.paths = append(f.paths, r.URL.Path)
			f.bodies = append(f.bodies, string(raw))
			return &http.Response{
				StatusCode: f.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(f.body)),
			}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

// embedOKBody 上游成功形态：data[] 里**没有** object/index（由网关补齐）。
const embedOKBody = `{"code":0,"msg":"OK","model":"bge-m3","data":[` +
	`{"embedding":[0.1,0.2]},{"embedding":[0.3,0.4]}],"usage":{"prompt_tokens":6,"total_tokens":6}}`

func embedHandler(t *testing.T, f *embedFake, enabled bool) *Handler {
	t.Helper()
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	return NewHandler(Config{Pool: p, Upstream: f.client(), EmbeddingsEnabled: enabled})
}

func postEmbeddings(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEmbeddingsRespectsModelPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		allowed, hidden []string
		want            int
	}{
		{"allowlist rejects", `{"model":"bge-m3","input":["a","b"]}`, []string{"cn:other"}, nil, 400},
		{"hidden rejects", `{"model":"cn:bge-m3","input":["a","b"]}`, nil, []string{"cn:bge-m3"}, 400},
		{"explicit realm rejects", `{"model":"global:bge-m3","realm":"cn","input":["a","b"]}`, []string{"global:bge-m3"}, nil, 400},
		{"allowed succeeds", `{"model":"bge-m3","input":["a","b"]}`, []string{"cn:bge-m3"}, nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &embedFake{status: 200, body: embedOKBody}
			h := embedHandler(t, f, true)
			h.cfg.ModelsAllowed, h.cfg.ModelsHidden = tc.allowed, tc.hidden
			rec := postEmbeddings(h, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != 200 && len(f.paths) != 0 {
				t.Fatal("rejected model reached upstream")
			}
		})
	}
}

func TestEmbeddingsRejectsMalformedVectors(t *testing.T) {
	for _, data := range []string{`[null]`, `[{}]`, `[{"embedding":null}]`, `[{"embedding":[]}]`, `[{"embedding":["bad"]}]`} {
		t.Run(data, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("malformed upstream response panicked: %v", p)
				}
			}()
			f := &embedFake{status: 200, body: `{"data":` + data + `}`}
			rec := postEmbeddings(embedHandler(t, f, true), `{"model":"bge-m3","input":"a"}`)
			if rec.Code != 502 {
				t.Fatalf("status=%d want=502: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestEmbeddingsRejectsOversizedBody(t *testing.T) {
	f := &embedFake{status: 200, body: embedOKBody}
	body := `{"model":"bge-m3","input":["a","b"]}`
	// A valid JSON prefix followed by whitespace must not hide an oversized body.
	body += strings.Repeat(" ", (8<<20)+1-len(body))
	rec := postEmbeddings(embedHandler(t, f, true), body)
	if rec.Code != 413 || len(f.paths) != 0 {
		t.Fatalf("status=%d upstream calls=%d", rec.Code, len(f.paths))
	}
}

func TestEmbeddingsPreservesUpstreamIndices(t *testing.T) {
	f := &embedFake{status: 200, body: `{"data":[{"index":1,"embedding":[0.2]},{"index":0,"embedding":[0.1]}]}`}
	rec := postEmbeddings(embedHandler(t, f, true), `{"model":"bge-m3","input":["a","b"]}`)
	var got struct {
		Data []struct {
			Index     int
			Embedding []float64
		}
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
	}
	if got.Data[0].Index != 1 || got.Data[0].Embedding[0] != 0.2 || got.Data[1].Index != 0 {
		t.Fatalf("vectors were assigned to the wrong input: %s", rec.Body.String())
	}
	for _, data := range []string{
		`[{"index":2,"embedding":[0.1]}]`,
		`[{"index":0.5,"embedding":[0.1]}]`,
		`[{"index":null,"embedding":[0.1]}]`,
	} {
		f.body = `{"data":` + data + `}`
		if rec := postEmbeddings(embedHandler(t, f, true), `{"model":"bge-m3","input":"a"}`); rec.Code != 502 {
			t.Errorf("accepted invalid index: %s", rec.Body.String())
		}
	}
	f.body = `{"data":[{"index":0,"embedding":[0.1]},{"index":0,"embedding":[0.2]}]}`
	if rec := postEmbeddings(embedHandler(t, f, true), `{"model":"bge-m3","input":["a","b"]}`); rec.Code != 502 {
		t.Errorf("accepted duplicate index: %s", rec.Body.String())
	}
}

func TestEmbeddingsQueueDoesNotRotate(t *testing.T) {
	// 6020/6021 是全局队列：换号只会多撞一次墙。终态 429（与 chat 同口径），不罚号不轮转。
	for _, status := range []int{200, 429} {
		f := &embedFake{status: status, body: `{"code":6020,"msg":"queue.waiting.title","data":{"retry_after":40}}`}
		rec := postEmbeddings(embedHandler(t, f, true), `{"model":"bge-m3","input":"a"}`)
		if rec.Code != 429 || len(f.paths) != 1 {
			t.Errorf("queue status=%d: response=%d calls=%d", status, rec.Code, len(f.paths))
		}
		if got := rec.Header().Get("Retry-After"); got != "40" {
			t.Errorf("queue status=%d: Retry-After=%q want 40", status, got)
		}
	}
}

// TestEmbeddingsTeiTooLargeDoesNotRotate 11353（TEI 返回 413、上游套 500 外壳）是
// **请求内容**的问题：同一 body 换任何账号都是同一句。落 ErrServer 会为一条超大
// input 轮转 3 个号并各喂一笔连败 —— issue #290 实测 15 个坏请求熔断 11 个健康号
// 30 分钟（chat 一起停）。现在是请求级 fail-fast：只打一次上游、不罚号、回 4xx
// （500 外壳原样透出会诱导 SDK 自动重试同一条坏 body）。
func TestEmbeddingsTeiTooLargeDoesNotRotate(t *testing.T) {
	const body11353 = `{"code":11353,"msg":"Generate embedding failed with error: 11353:generate bemdding by tei failed with status code: [413]"}`
	f := &embedFake{status: 500, body: body11353}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c3", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f.client(), EmbeddingsEnabled: true})
	rec := postEmbeddings(h, `{"model":"bge-m3","input":"a"}`)

	if len(f.paths) != 1 {
		t.Fatalf("请求级错误只该打一次上游，实际 %d 次", len(f.paths))
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应回 400（上游 500 只是外壳），实得 %d: %s", rec.Code, rec.Body.String())
	}
	for _, st := range p.List() {
		if st.CoolRemaining > 0 || !st.BreakerUntil.IsZero() || !st.DegradeUntil.IsZero() {
			t.Errorf("账号 %s 被冷却：cool_remaining=%d breaker_until=%v degrade_until=%v",
				st.UID, st.CoolRemaining, st.BreakerUntil, st.DegradeUntil)
		}
		if st.ErrTotal != 0 || st.BreakerFails != 0 || st.ConsecutiveFails != 0 {
			t.Errorf("账号 %s 的失败计数被动过：err_total=%d breaker_fails=%d consecutive_fails=%d",
				st.UID, st.ErrTotal, st.BreakerFails, st.ConsecutiveFails)
		}
	}
}

// TestEmbeddingsDisabledByDefault 默认关闭：回 403 embeddings_disabled 且不打上游。
//
// 为什么不是 404：404 会让调用方以为「路径拼错了」，而实际是功能存在、只是没开。
func TestEmbeddingsDisabledByDefault(t *testing.T) {
	f := &embedFake{status: 200, body: embedOKBody}
	h := embedHandler(t, f, false)
	rec := postEmbeddings(h, `{"model":"bge-m3","input":"你好"}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("默认应 403，实得 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("错误体不是 JSON: %v", err)
	}
	if resp.Error.Code != "embeddings_disabled" {
		t.Errorf("code=%q want embeddings_disabled", resp.Error.Code)
	}
	if len(f.paths) != 0 {
		t.Errorf("关闭时不得打上游，实际 %d 次", len(f.paths))
	}
}

// TestEmbeddingsHappyPath 单条字符串输入：上游只认数组 → 包成一条；
// 回程补 object/index（上游不返回），model/usage 原样保留。
func TestEmbeddingsHappyPath(t *testing.T) {
	f := &embedFake{status: 200, body: embedOKBody}
	h := embedHandler(t, f, true)
	rec := postEmbeddings(h, `{"model":"bge-m3","input":["你好","世界"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.paths) != 1 || f.paths[0] != "/v2/embeddings" {
		t.Fatalf("上游路径=%v want [/v2/embeddings]", f.paths)
	}
	var sent struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	if sent.Model != "bge-m3" || len(sent.Input) != 2 {
		t.Errorf("上游请求体=%s want model=bge-m3 input 两条", f.bodies[0])
	}

	var got struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body.String())
	}
	if got.Object != "list" {
		t.Errorf("顶层 object=%q want list", got.Object)
	}
	if got.Model != "bge-m3" || got.Usage.TotalTokens != 6 {
		t.Errorf("model/usage 未原样保留: %+v", got)
	}
	for i, d := range got.Data {
		if d.Object != "embedding" || d.Index != i {
			t.Errorf("data[%d] object=%q index=%d want embedding/%d", i, d.Object, d.Index, i)
		}
	}
}

// TestEmbeddingsModelNotFoundMaps404 上游 11351（HTTP 500）= 模型不存在，
// 必须映射成 OpenAI 的 404，并且在文案里点出已知可用模型——直接回 500 会让调用方去查服务健康。
func TestEmbeddingsModelNotFoundMaps404(t *testing.T) {
	f := &embedFake{status: 500, body: `{"code":11351,"msg":"embedding model [nope] not found"}`}
	h := embedHandler(t, f, true)
	rec := postEmbeddings(h, `{"model":"nope","input":"你好"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("11351 应映射 404，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), upstream.EmbeddingModelBGE) {
		t.Errorf("文案应点出可用模型 %s: %s", upstream.EmbeddingModelBGE, rec.Body.String())
	}
	// 模型不存在与账号无关：不得换号重试。
	if len(f.paths) != 1 {
		t.Errorf("模型不存在不该换号重试，实际打了 %d 次", len(f.paths))
	}
}

// TestEmbeddingsBadInput 输入形态非法一律 400，且不打上游。
func TestEmbeddingsBadInput(t *testing.T) {
	cases := []struct{ name, body string }{
		{"缺 model", `{"input":"你好"}`},
		{"缺 input", `{"model":"bge-m3"}`},
		{"空串", `{"model":"bge-m3","input":""}`},
		{"空数组", `{"model":"bge-m3","input":[]}`},
		{"数组含非字符串", `{"model":"bge-m3","input":["a",1]}`},
		{"分词数组", `{"model":"bge-m3","input":[[1,2,3]]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &embedFake{status: 200, body: embedOKBody}
			h := embedHandler(t, f, true)
			rec := postEmbeddings(h, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400: %s", rec.Code, rec.Body.String())
			}
			if len(f.paths) != 0 {
				t.Errorf("非法请求不得打上游，实际 %d 次", len(f.paths))
			}
		})
	}
}

// TestEmbeddingsCountMismatchIs502 上游返回条数与输入不符：错位配对比报错更难查，
// 一律 502 拒绝（不透传错位向量）。
func TestEmbeddingsCountMismatchIs502(t *testing.T) {
	f := &embedFake{status: 200, body: `{"code":0,"model":"bge-m3","data":[{"embedding":[0.1]}],` +
		`"usage":{"total_tokens":3}}`}
	h := embedHandler(t, f, true)
	rec := postEmbeddings(h, `{"model":"bge-m3","input":["a","b"]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("条数不符应 502，实得 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEmbeddingsRealmPrefixStripped cn: / global: 前缀只用于选号域，出站必须剥掉
// （上游目录里的模型 id 不带域前缀）。
func TestEmbeddingsRealmPrefixStripped(t *testing.T) {
	f := &embedFake{status: 200, body: `{"code":0,"model":"bge-m3","data":[{"embedding":[0.1]}],` +
		`"usage":{"total_tokens":3}}`}
	h := embedHandler(t, f, true)
	rec := postEmbeddings(h, `{"model":"cn:bge-m3","input":"你好"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	if sent.Model != "bge-m3" {
		t.Errorf("出站模型名=%q want bge-m3（前缀必须剥掉）", sent.Model)
	}
}
