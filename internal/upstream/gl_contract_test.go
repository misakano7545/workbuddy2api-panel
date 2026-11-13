// gl_contract_test.go GL 客户端请求契约回归（黄金头集来自 WorkBuddy AI 5.9.4 真机抓包）。
//
// 与 cn_contract_test.go 同构，但锁的是**国际版 5.9.4** 的出站形态。本文件覆盖三处
// 2026-10-30 抓包才暴露、且与 CN 不同形的差异：
//
//  1. X-Private-Data：国际发行版 productFeatures.DefaultModelOptimizationOff=true
//     （国内 5.9.3 是 false）→ 个人号声明 true。旧实现按企业/个人判，方向反了。
//  2. RPC/REST 族 UA 首段是品牌 slug（workbuddy-ai/…），chat 族首段才是驼峰
//     WorkBuddy/… —— 同一会话内两族并存，是客户端真实形态。
//  3. baggage 只有 4 个成员。
//
// 抓包环境：WorkBuddy AI 5.9.4（CLI 2.165.0），个人号，www.workbuddy.ai。
// 取每条路径全部样本的交集；连接层/传输层头与 CN 侧同规则剔除。
package upstream

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// glGlobalAuth 国际版个人号（Domain 落 www.workbuddy.ai，与真机 X-Domain 同值）。
func glGlobalAuth() *auth.Auth {
	return &auth.Auth{
		UID: "00000000-0000-4000-8000-000000000001", AccessToken: "AT",
		RefreshToken: "RT", Nickname: "misakano7545",
		Domain: "www.workbuddy.ai",
	}
}

var glGoldenCases = []cnGolden{
	{
		// 真机 idx 4 条 /v2/chat/completions 同构。
		path: "/v2/chat/completions",
		headers: []string{
			"accept", "authorization", "acp-connection-id", "b3", "baggage",
			"content-type", "traceparent", "user-agent",
			"x-agent-intent", "x-agent-purpose", "x-agent-runtime", "x-agent-type",
			"x-b3-parentspanid", "x-b3-sampled", "x-b3-spanid", "x-b3-traceid",
			"x-codebuddy-request", "x-conversation-id", "x-conversation-message-id",
			"x-conversation-request-id", "x-domain", "x-ide-name", "x-ide-type",
			"x-ide-version", "x-private-data", "x-product", "x-request-id",
			"x-requested-with", "x-root-request-id", "x-trace-id", "x-user-id",
			"x-stainless-arch", "x-stainless-lang", "x-stainless-os",
			"x-stainless-package-version", "x-stainless-retry-count",
			"x-stainless-runtime", "x-stainless-runtime-version",
		},
		values: map[string]string{
			"accept":              "application/json",
			"user-agent":          "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0",
			"x-private-data":      "true",
			"x-domain":            "www.workbuddy.ai",
			"x-ide-name":          "WorkBuddy",
			"x-ide-type":          "WorkBuddy",
			"x-ide-version":       "5.9.4",
			"x-product":           "SaaS",
			"x-agent-intent":      "craft",
			"x-agent-purpose":     "conversation",
			"x-agent-runtime":     "local-agent",
			"x-agent-type":        "main",
			"x-requested-with":    "XMLHttpRequest",
			"x-codebuddy-request": "1",
		},
		absent: []string{"origin", "referer", "accept-language", "x-enterprise-id"},
	},
	{
		// 真机 /v2/chat/busy：RPC 族，UA 首段是 slug。
		// X-Domain 的值由 chat base 推出（本测试指到 httptest），只断言键存在。
		path: "/v2/chat/busy",
		headers: []string{
			"accept", "authorization", "content-type", "user-agent",
			"x-domain", "x-product", "x-request-id", "x-user-id",
		},
		values: map[string]string{
			"accept":     "application/json, text/plain, */*",
			"user-agent": "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0",
			"x-product":  "SaaS",
		},
		absent: []string{"origin", "referer", "x-codebuddy-request", "x-private-data", "x-requested-with"},
	},
	{
		// 真机 /v2/report：桌面端上报，同样是 RPC 族 UA。
		// 取**已登录**样本的头集（登录前那几条没有 x-domain / x-user-id）：
		//   accept / authorization / content-type / user-agent / x-domain /
		//   x-product / x-request-id / x-user-id，且无 X-Requested-With。
		path: "/v2/report",
		headers: []string{
			"accept", "authorization", "content-type", "user-agent",
			"x-domain", "x-product", "x-request-id", "x-user-id",
		},
		values: map[string]string{
			"user-agent": "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0",
			"x-product":  "SaaS",
		},
		absent: []string{"origin", "referer", "x-codebuddy-request", "x-requested-with"},
	},
	{
		// 真机 /v3/config：两段单品牌，恒不带 x-codebuddy-request。
		// XRW：RPC 层不打此头（见 headers.go CommonHeaders 注释）。
		path: "/v3/config",
		headers: []string{
			"accept", "authorization", "user-agent", "x-domain",
			"x-product", "x-request-id", "x-user-id",
		},
		values: map[string]string{
			"accept":     "application/json, text/plain, */*",
			"user-agent": "WorkBuddy/5.9.4 CLI/2.165.0",
			"x-domain":   "www.workbuddy.ai",
			"x-product":  "SaaS",
		},
		absent: []string{"origin", "referer", "accept-language", "x-codebuddy-request"},
	},
}

// TestGLContractGolden 用生产代码真实构造 GL 四条关键路径，逐条比对黄金头集。
func TestGLContractGolden(t *testing.T) {
	captured := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" && len(raw) > 2 && raw[0] == 0x1f {
			if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
				raw, _ = io.ReadAll(zr)
			}
		}
		if _, seen := captured[r.URL.Path]; !seen {
			captured[r.URL.Path] = r.Header.Clone()
		}
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	a := glGlobalAuth()
	c := &Client{
		HTTP: srv.Client(), ChatHTTP: srv.Client(),
		ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGlobal: srv.URL,
		GlobalEnabled: true,
	}
	c.SanitizeFingerprints.Store(true)
	c.SanitizeBrandScope.Store("all")

	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	meta := ChatMeta{
		ConversationID:        "0173f340-72d0-485a-8940-06c8ab3b5e8d",
		ConversationRequestID: "01a18b7d9dc479fc8d872ebc6b07665f",
	}
	if rc, _, _, err := c.ChatStream(a, body, "", meta); err == nil && rc != nil {
		io.Copy(io.Discard, rc)
		rc.Close()
	}
	_ = c.ReportChatActivity(a, "0173f340-72d0-485a-8940-06c8ab3b5e8d", "01a18b7d9dc479fc8d872ebc6b07665f")
	_, _ = c.ModelBusy(a, []string{"deepseek-v4.1-flash"})
	_, _, _ = c.fetchV3ConfigModelMap(a, "")

	for _, g := range glGoldenCases {
		t.Run(strings.TrimPrefix(g.path, "/"), func(t *testing.T) {
			hdr, ok := captured[g.path]
			if !ok {
				t.Fatalf("未捕获到 %s 的出站请求", g.path)
			}
			got := map[string]bool{}
			for k := range hdr {
				lk := strings.ToLower(k)
				if !transportOnly[lk] {
					got[lk] = true
				}
			}
			want := map[string]bool{}
			for _, k := range g.headers {
				want[k] = true
			}
			var extra, missing []string
			for k := range got {
				if !want[k] {
					extra = append(extra, k)
				}
			}
			for k := range want {
				if !got[k] {
					missing = append(missing, k)
				}
			}
			sort.Strings(extra)
			sort.Strings(missing)
			if len(extra) > 0 {
				t.Errorf("多发了客户端没有的头: %v", extra)
			}
			if len(missing) > 0 {
				t.Errorf("漏发了真机必带的头: %v", missing)
			}
			for k, v := range g.values {
				if got := hdr.Get(k); got != v {
					t.Errorf("%s=%q want %q", k, got, v)
				}
			}
			for _, k := range g.absent {
				if got := hdr.Get(k); got != "" {
					t.Errorf("%s 不应出现（真机无），实际=%q", k, got)
				}
			}
		})
	}
}

// TestGLBaggageExactMembers 真机 baggage 恰好 4 个成员，顺序固定，两个 buddy 键留空值。
func TestGLBaggageExactMembers(t *testing.T) {
	c := &Client{}
	req := httptest.NewRequest("POST", "https://www.workbuddy.ai/v2/chat/completions", nil)
	c.ChatHeaders(req, glGlobalAuth(), "", ChatMeta{
		ConversationID:        "0173f340-72d0-485a-8940-06c8ab3b5e8d",
		ConversationRequestID: "01a18b7d9dc479fc8d872ebc6b07665f",
	})
	want := "buddyId=,buddyName=,codebuddy.session_id=0173f340-72d0-485a-8940-06c8ab3b5e8d," +
		"codebuddy.conversation_request_id=01a18b7d9dc479fc8d872ebc6b07665f"
	if got := req.Header.Get("baggage"); got != want {
		t.Errorf("baggage 成员/顺序不符\n got=%q\nwant=%q", got, want)
	}
}
