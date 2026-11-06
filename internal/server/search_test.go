package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// searchFake 记录检索落点/体，并按脚本回状态与原文。
type searchFake struct {
	paths  []string
	hosts  []string
	bodies []string
	status int
	body   string
}

func (f *searchFake) client() *upstream.Client {
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
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

const searchOKBody = `{"query":"玉林 电梯 维保","type":"text2text","provider":"0",` +
	`"results":[{"title":"新规","url":"https://x.example/1","snippet":"..."}],` +
	`"total_results":5,"response_time_ms":631}`

// dshUA DSH（DeepSeek Harness）的实测 User-Agent；兼容层只对含
// "deepseek-harness" 的请求启用。
const dshUA = "deepseek-harness/0.2.1-rc.2 (+https://github.com/deepseek-ai/deepseek-harness)"

// TestSearchEndpoint POST /v1/search：原文透传、缺 query 400、请求级 400 不轮转。
func TestSearchEndpoint(t *testing.T) {
	f := &searchFake{status: 200, body: searchOKBody}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	postUA := func(body, ua string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/search", strings.NewReader(body))
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// 默认按 DSH 客户端发（兼容层的目标读者）。
	post := func(body string) *httptest.ResponseRecorder { return postUA(body, dshUA) }

	// ① 成功：上游字段语义不变（仅补 DeepSeek Search API 需要的 date:null），落点在 CN 域
	rec := post(`{"query":"玉林 电梯 维保","max_results":5}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Query        string           `json:"query"`
		Type         string           `json:"type"`
		Provider     string           `json:"provider"`
		Results      []map[string]any `json:"results"`
		TotalResults int              `json:"total_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if got.Query != "玉林 电梯 维保" || got.Type != "text2text" || got.Provider != "0" || got.TotalResults != 5 {
		t.Fatalf("上游字段透传不符: %+v", got)
	}
	if len(got.Results) != 1 || got.Results[0]["url"] != "https://x.example/1" ||
		got.Results[0]["title"] != "新规" || got.Results[0]["snippet"] != "..." {
		t.Fatalf("结果项透传不符: %+v", got.Results)
	}
	// date 必须存在且为 null：DSH 的 deepseek-search-api 用 `item.date !== null &&
	// typeof item.date !== 'string'` 校验，缺字段（undefined）会直接判 invalidResponse。
	if v, ok := got.Results[0]["date"]; !ok || v != nil {
		t.Fatalf("date 需为显式 null，得到 ok=%v v=%v", ok, v)
	}
	if f.paths[0] != "/agenttool/v1/search" || f.hosts[0] != "cn.example" {
		t.Fatalf("落点 %s%s", f.hosts[0], f.paths[0])
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	if sent["type"] != "text2text" || sent["max_results"] != float64(5) {
		t.Fatalf("请求体归一不符: %v", sent)
	}

	// ①b DeepSeek 形状只发 count（不发 max_results）：要回落生效，不能静默按默认 5 条
	f3 := &searchFake{status: 200, body: searchOKBody}
	h3 := NewHandler(Config{Pool: p, Upstream: f3.client()})
	req3 := httptest.NewRequest("POST", "/v1/search",
		strings.NewReader(`{"query":"x","count":3,"need_content":false}`))
	req3.Header.Set("User-Agent", dshUA)
	rec = httptest.NewRecorder()
	h3.ServeHTTP(rec, req3)
	if rec.Code != 200 {
		t.Fatalf("count 请求 code=%d body=%s", rec.Code, rec.Body.String())
	}
	var sentCount map[string]any
	_ = json.Unmarshal([]byte(f3.bodies[0]), &sentCount)
	if sentCount["max_results"] != float64(3) {
		t.Fatalf("count 未回落为 max_results: %v", sentCount)
	}

	// ①c 非 DSH 客户端：兼容层必须完全不介入——响应逐字节等于上游原文（不补 date、
	// 不重排字段），count 也不参与条数（仍由上游默认 5 条兜底）。
	f4 := &searchFake{status: 200, body: searchOKBody}
	h4 := NewHandler(Config{Pool: p, Upstream: f4.client()})
	req4 := httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x","count":3}`))
	req4.Header.Set("User-Agent", "curl/8.23.0")
	rec = httptest.NewRecorder()
	h4.ServeHTTP(rec, req4)
	if rec.Code != 200 {
		t.Fatalf("非 DSH 请求 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != searchOKBody {
		t.Fatalf("非 DSH 响应应逐字节透传，得到 %s", got)
	}
	var sentPlain map[string]any
	_ = json.Unmarshal([]byte(f4.bodies[0]), &sentPlain)
	if sentPlain["max_results"] != float64(5) {
		t.Fatalf("非 DSH 的 count 不应回落（应走上游默认 5）: %v", sentPlain)
	}

	// ② 缺 query：400 且不打上游（少一次白烧）
	before := len(f.paths)
	if rec := post(`{"query":"  "}`); rec.Code != 400 {
		t.Fatalf("空 query 应 400，得到 %d", rec.Code)
	}
	if len(f.paths) != before {
		t.Fatal("空 query 不应发出上游请求")
	}

	// ③ 请求级 400（15003）：原文透传、只打 1 次上游（不轮转烧第二个号）
	f2 := &searchFake{status: 400, body: `{"code":15003,"msg":"unsupported search provider: 0"}`}
	h2 := NewHandler(Config{Pool: p, Upstream: f2.client()})
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "15003") {
		t.Fatalf("请求级 400 应原文透传，得到 %d %s", rec.Code, rec.Body.String())
	}
	if len(f2.paths) != 1 {
		t.Fatalf("请求级错误应 fail-fast 不轮转，实际打了 %d 次", len(f2.paths))
	}

	// ④ 无可用账号：503 no_account（不是含糊的 502）
	empty := NewHandler(Config{Pool: testPoolWith(), Upstream: f.client()})
	rec = httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "no_account") {
		t.Fatalf("空池应 503 no_account，得到 %d %s", rec.Code, rec.Body.String())
	}
}

// TestSearchDeepSwitch deep:true 走 agentic_search（综述原文），缺省仍走 snippet 快路径。
func TestSearchDeepSwitch(t *testing.T) {
	f := &searchFake{status: 200, body: `{"query":"x","results":[{"title":"t","url":"https://x/1","snippet":"s"}]}`}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != 200 || f.paths[0] != "/agenttool/v1/search" {
		t.Fatalf("缺省应走 snippet 快路径: code=%d path=%v", rec.Code, f.paths)
	}

	// deep:true → agentic_search，且 done 帧原文透出（synthesis/content 都在）
	agentic := `event: done` + "\n" + `data: {"partial":true,"partial_reason":"timeout",` +
		`"synthesis":{"content":"综述正文","inline_content":"综述"},"results":[{"id":"r1","content":"正文"}]}` + "\n\n"
	f2 := &searchFake{status: 200, body: agentic}
	h2 := NewHandler(Config{Pool: p, Upstream: f2.client()})
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x","deep":true}`)))
	if rec.Code != 200 {
		t.Fatalf("deep code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f2.paths[0] != "/agenttool/v1/agentic_search" {
		t.Fatalf("deep 应落 agentic_search: %v", f2.paths)
	}
	var done map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("deep 响应不是 done 原文: %v (%s)", err, rec.Body.String())
	}
	if syn, _ := done["synthesis"].(map[string]any); syn["content"] != "综述正文" {
		t.Fatalf("综述未透传: %v", done)
	}
	// partial 是上游自己的降级标记，必须让调用方看见（别把降级当完整）
	if done["partial"] != true || done["partial_reason"] != "timeout" {
		t.Fatalf("partial 未透传: %v", done)
	}
}

// TestCompatDeepSeekSearch 只补 date:null，其余字段值原样保留；坏输入不篡改。
//
// DSH（deepseek-harness）的 deepseek-search-api provider 对响应校验很硬：
// results[].date 必须是 null 或 string，字段缺失（undefined）直接判
// invalidResponse —— 线上表现为 “DeepSeek returned an invalid response body”。
func TestCompatDeepSeekSearch(t *testing.T) {
	in := `{"query":"q","results":[{"title":"a","url":"https://x/1","snippet":"s"},` +
		`{"title":"b","url":"https://x/2","snippet":"","date":"2026-01-02"},` +
		`{"url":"https://x/3","snippet":null}]}`
	var out map[string]any
	if err := json.Unmarshal(compatDeepSeekSearch([]byte(in)), &out); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	items, _ := out["results"].([]any)
	if len(items) != 3 {
		t.Fatalf("results 数量变了: %v", out)
	}
	first, _ := items[0].(map[string]any)
	if v, ok := first["date"]; !ok || v != nil {
		t.Fatalf("缺 date 的项应补 null: ok=%v v=%v", ok, v)
	}
	if first["url"] != "https://x/1" || first["snippet"] != "s" || first["title"] != "a" {
		t.Fatalf("其余字段应原样保留: %v", first)
	}
	second, _ := items[1].(map[string]any)
	if second["date"] != "2026-01-02" {
		t.Fatalf("已有 date 应原样保留: %v", second)
	}
	// 缺 title / snippet（或为 null）时补空串：DSH 对两者也要求 string
	third, _ := items[2].(map[string]any)
	if third["title"] != "" || third["snippet"] != "" || third["date"] != nil {
		t.Fatalf("缺 title/snippet 的项应补空串、date 补 null: %v", third)
	}

	// 无法安全改写时原样返回，绝不把好响应改坏
	for _, raw := range []string{`not json`, `{"query":"q"}`, `{"results":[]}`, `{"results":"x"}`, ``} {
		if got := string(compatDeepSeekSearch([]byte(raw))); got != raw {
			t.Fatalf("应原样返回 %q，得到 %q", raw, got)
		}
	}
}

// TestDeepSeekSearchClientDetection 兼容层只认 DSH 的 UA（窄子串、大小写不敏感），
// 其余客户端（含空 UA）一律走原文透传。
func TestDeepSeekSearchClientDetection(t *testing.T) {
	for _, ua := range []string{
		dshUA,
		"DeepSeek-Harness/0.3.0",
		"deepseek-harness",
	} {
		if !deepSeekSearchClient(ua) {
			t.Errorf("UA %q 应识别为 DSH", ua)
		}
	}
	for _, ua := range []string{
		"",
		"curl/8.23.0",
		"python-requests/2.32",
		"deepseek-search-api/1.0",
		"CodeBuddy/2.63.2",
	} {
		if deepSeekSearchClient(ua) {
			t.Errorf("UA %q 不应识别为 DSH", ua)
		}
	}
}

// searchCardBody 复刻线上实测响应：第 2 条是上游「结构化卡片」——有标题、有摘要
// （答案引擎生成的表格），但 url 为空，因为它不是网页。其余两条是正常网页条目。
const searchCardBody = `{"query":"宁德时代 股价","type":"text2text","provider":"0","results":[` +
	`{"title":"宁德时代 300750","url":"https://quote.example/300750","snippet":"实时行情"},` +
	`{"title":"港股行情指标(货币-港元) | A股行情指标(货币-人民币)","url":"",` +
	`"site":"腾讯自选股","favicon":"https://search.imtt.qq.com/x.png",` +
	`"snippet":"## 港股行情指标\n| hk03750 | 宁德时代 | 380.00 | 2026-11-06 |"},` +
	`{"title":"宁德时代 年报","url":"https://x.example/2","snippet":"..."}],` +
	`"total_results":3,"response_time_ms":512}`

// TestSearchCompatCardRewrite POST /v1/search 对 DSH 的卡片改写。
//
// 上游会把「结构化卡片」混进 results（url 为空、snippet 是表格）。DSH 官方插件
// （@deepseek-ai/dsh-web-deepseek-api）对每一要求 url 非空字符串，一条不满足就整包
// 抛 invalidResponse —— 线上表现为 “DeepSeek returned an invalid response body”，
// 同一批里 7 条合法网页一起作废。
//
// 兼容层把这类条目改写成一次同词检索的链接：snippet 原样保留（卡片里的表格仍能被
// 模型读到），只有连取词都没有的才丢弃。改写只发生在 DSH 分支，其余客户端仍逐字节
// 透传，卡片原样带着空 url 出去。
func TestSearchCompatCardRewrite(t *testing.T) {
	postRaw := func(ua, body string) *httptest.ResponseRecorder {
		f := &searchFake{status: 200, body: body}
		h := NewHandler(Config{
			Pool: testPoolWith(
				&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
			),
			Upstream: f.client(),
		})
		req := httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"宁德时代 股价","count":8}`))
		req.Header.Set("User-Agent", ua)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// ① 有 query：卡片 url 改写成「同词检索」，snippet/title 原样，条数不变
	rec := postRaw(dshUA, searchCardBody)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results      []map[string]any `json:"results"`
		TotalResults int              `json:"total_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if len(got.Results) != 3 || got.TotalResults != 3 {
		t.Fatalf("条数不应变化: len=%d total=%d", len(got.Results), got.TotalResults)
	}
	wantURL := cardSearchBase + url.QueryEscape("宁德时代 股价")
	if got.Results[1]["url"] != wantURL {
		t.Fatalf("卡片 url 应被改写为 %q，得到 %v", wantURL, got.Results[1]["url"])
	}
	if !strings.Contains(got.Results[1]["snippet"].(string), "hk03750") {
		t.Fatalf("卡片的表格摘要必须保留: %v", got.Results[1]["snippet"])
	}
	if got.Results[1]["title"] != "港股行情指标(货币-港元) | A股行情指标(货币-人民币)" {
		t.Fatalf("卡片标题应原样保留: %v", got.Results[1]["title"])
	}
	if got.Results[0]["url"] != "https://quote.example/300750" {
		t.Fatalf("正常条目的 url 不应被改: %v", got.Results[0]["url"])
	}
	// 官方插件的校验谓词：改完之后每一条都必须能过，否则整包还是会被作废
	assertDSHAcceptsResults(t, rec.Body.Bytes())

	// ② 响应里没有 query：退回卡片标题取词（上游 deep 形态等可能不带 query）
	rec = postRaw(dshUA, `{"results":[{"title":"广西-录取分数线","url":"","snippet":"| 省份 |"}]}`)
	var fb struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fb); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	wantTitleURL := cardSearchBase + url.QueryEscape("广西-录取分数线")
	if len(fb.Results) != 1 || fb.Results[0]["url"] != wantTitleURL {
		t.Fatalf("无 query 时应按标题取词，期望 %q，得到 %v", wantTitleURL, fb.Results)
	}

	// ③ 连取词都没有（query 与 title 皆空）：丢弃该条并修正 total_results
	rec = postRaw(dshUA, `{"query":"","results":[`+
		`{"title":"","url":"","snippet":"x"},`+
		`{"title":"正常","url":"https://x.example/1","snippet":"y"}],`+
		`"total_results":2}`)
	var dropped struct {
		Results      []map[string]any `json:"results"`
		TotalResults int              `json:"total_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dropped); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if len(dropped.Results) != 1 || dropped.TotalResults != 1 {
		t.Fatalf("无法改写的条目应被丢弃且 total_results 同步: %v total=%d",
			dropped.Results, dropped.TotalResults)
	}
	assertDSHAcceptsResults(t, rec.Body.Bytes())

	// ④ 非 DSH 客户端：逐字节透传，卡片保持原样（兼容层不介入）
	rec = postRaw("curl/8.23.0", searchCardBody)
	if got := rec.Body.String(); got != searchCardBody {
		t.Fatalf("非 DSH 响应应逐字节透传，得到 %s", got)
	}
}

// assertDSHAcceptsResults 用 DSH 官方插件（@deepseek-ai/dsh-web-deepseek-api）的校验
// 谓词检查一份 /v1/search 响应，等价于它的 mapSearchResponse：
//
//	item 是对象；url 是非空 string；title 是 string；snippet 是 string；
//	date 是 null 或 string（字段缺失即 undefined，会被判非法）。
//
// 任一条不满足，插件抛 invalidResponse，整次检索作废——所以这条断言比逐字段比对
// 更能钉住“客户端不会再整包失败”。
func assertDSHAcceptsResults(t *testing.T, body []byte) {
	t.Helper()
	var payload struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	for i, it := range payload.Results {
		if it == nil {
			t.Fatalf("results[%d] 不是对象", i)
		}
		var s string
		if err := json.Unmarshal(it["url"], &s); err != nil || s == "" {
			t.Fatalf("results[%d].url 必须是非空 string：%s", i, it["url"])
		}
		if err := json.Unmarshal(it["title"], &s); err != nil {
			t.Fatalf("results[%d].title 必须是 string：%s", i, it["title"])
		}
		if err := json.Unmarshal(it["snippet"], &s); err != nil {
			t.Fatalf("results[%d].snippet 必须是 string：%s", i, it["snippet"])
		}
		raw, has := it["date"]
		if !has {
			t.Fatalf("results[%d] 缺 date 字段（官方插件判 invalidResponse）", i)
		}
		if string(raw) != "null" {
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatalf("results[%d].date 必须是 null 或 string：%s", i, raw)
			}
		}
	}
}
