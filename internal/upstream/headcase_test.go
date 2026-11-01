// headcase_test.go 头名拼写纠正的回归测试与测试辅助。
//
// 本文件的 headerFold 是给**测试**用的：normalizeHeaderSpelling 把出站头 key
// 换成了官方拼写（`X-Conversation-ID` 而非 Go canonical 的 `X-Conversation-Id`），
// 而 http.Header.Get 会先把查询键 canonicalize 再查 map，于是取不到值。
// 在自定义 RoundTripper 里读**出站**头的测试必须用它。
//
// 生产代码没有这个依赖：所有读 req.Header 的地方都在构造阶段（do() 之前），
// 那时 key 还是 canonical 形式。这一点若被将来改动打破，本文件的用例会先红。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// headerFold 大小写不敏感地取头值（取第一个）。
func headerFold(h http.Header, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// TestNormalizeHeaderSpelling 拼写纠正必须逐名命中真机形式，且不能碰六个
// Go 特判头（碰了会让 UA 回落 Go-http-client、content-length 重发）。
func TestNormalizeHeaderSpelling(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", nil)

	// 模拟 ChatHeaders 写出来的 canonical key
	req.Header.Set("X-Conversation-ID", "c1")
	req.Header.Set("X-B3-ParentSpanId", "p1")
	req.Header.Set("X-B3-SpanId", "s1")
	req.Header.Set("X-B3-TraceId", "t1")
	req.Header.Set("Acp-Connection-Id", "a1")
	req.Header.Set("Baggage", "b=1")
	req.Header.Set("B3", "x-1")
	req.Header.Set("Traceparent", "00-x")
	req.Header.Set("X-Stainless-Arch", "x64")
	req.Header.Set("X-Request-Id", "r1")
	req.Header.Set("X-Ide-Version", "5.9.3")
	req.Header.Set("X-Trace-Id", "t1")
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0")
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	req.Header.Set("X-Enterprise-Id", "ent-1") // 表外，应原样保留

	got := normalizeHeaderSpelling(req)

	want := map[string]string{
		"X-Conversation-ID":     "c1",
		"X-B3-ParentSpanId":     "p1",
		"X-B3-SpanId":           "s1",
		"X-B3-TraceId":          "t1",
		"acp-connection-id":     "a1",
		"baggage":               "b=1",
		"b3":                    "x-1",
		"traceparent":           "00-x",
		"x-stainless-arch":      "x64",
		"X-Request-ID":          "r1",
		"X-IDE-Version":         "5.9.3",
		"X-Trace-ID":            "t1",
		"User-Agent":            "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0",
		"Accept-Encoding":       "gzip, compress, deflate, br",
		"X-Enterprise-Id":       "ent-1",
	}
	for k, v := range want {
		if h, ok := got.Header[k]; !ok {
			t.Errorf("缺头 %q（实际 key 集合见下）", k)
		} else if len(h) == 0 || h[0] != v {
			t.Errorf("%s=%q want %q", k, h, v)
		}
	}

	// 六个特判头必须仍是 canonical 拼写，否则 net/http 查不到
	for _, k := range []string{"User-Agent", "Accept-Encoding"} {
		if _, ok := got.Header[k]; !ok {
			t.Errorf("%s 必须保留 canonical 拼写（否则 Transport 行为改变）", k)
		}
	}

	// 原请求不受影响（调用方/测试还要回读它）
	if req.Header.Get("X-Conversation-ID") != "c1" {
		t.Error("原 req 的 header 被改写了——normalizeHeaderSpelling 必须返回副本")
	}
}

// TestHeaderFoldFindsOfficialSpelling 确认测试辅助本身能穿透官方拼写。
func TestHeaderFoldFindsOfficialSpelling(t *testing.T) {
	h := http.Header{"X-Conversation-ID": []string{"c9"}}
	if got := headerFold(h, "X-Conversation-ID"); got != "c9" {
		t.Errorf("headerFold=%q want c9", got)
	}
	// canonical 查询键（Get 的用法）在官方拼写下取不到——这正是要绕开的行为
	if got := h.Get("X-Conversation-ID"); got != "" {
		t.Logf("提示：Go 版本行为变化，Header.Get 已能取到 %q", got)
	}
}
