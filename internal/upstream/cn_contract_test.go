// cn_contract_test.go CN 客户端请求契约回归（黄金头集来自 WorkBuddy 5.9.3 真机抓包）。
//
// 这不是「按文档推的」测试，是把 2026-10-28 真机 mitmproxy 抓到的每个路径的**出站头
// 键集**固化成黄金值，再用生产代码真实构造请求逐条比对。任何一头多发/漏发都会红。
//
// 抓包环境：国内版 WorkBuddy 5.9.3（CLI 2.165.0），CN 账号，www.workbuddy.cn。
// 每个路径取的是该路径全部样本的**交集**（排除客户端按场景才加的偶发头，如 auxiliary
// 链才有的差异），再剔除三类与网关环境相关、不该进黄金集的键：
//
//	host / connection / content-length   ← 连接层，由 http.Client 决定
//	accept-encoding                      ← 传输层协商
//	x-device-token                       ← 账号是否配了设备令牌（配置项，非契约）
//
// 值断言只做「形态/关键常量」级（版本段、布尔值、枚举），动态 ID 只校验形态。
package upstream

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// cnGolden 单条路径的黄金契约。
type cnGolden struct {
	path string
	// headers 期望出现的出站头（全部小写）。必须**精确**等于实际集合（忽略上表三类）。
	headers []string
	// values 关键头的期望值（小写键）。
	values map[string]string
	// absent 明确断言**不能**出现的头（防回归：这些是旧实现多发的）。
	absent []string
}

var cnGoldenCases = []cnGolden{
	{
		// 真机 idx 438（另 6 条 /v2/chat/completions 同构）。
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
			"x-private-data":      "false", // 真机 false（模型优化开），不是 true
			"x-agent-runtime":     "local-agent",
			"x-agent-type":        "main",
			"x-codebuddy-request": "1",
			"x-ide-version":       "5.9.3",
			"x-product":           "SaaS",
			"user-agent":          "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0",
		},
		// 客户端 chat 从不上浏览器语义头（node fetch，无同源概念）。
		absent: []string{"origin", "referer", "accept-language", "x-machine-id", "x-session-id"},
	},
	{
		// 真机 idx 443。走 chat 域，UA 三段式（不是 billing 的 axios）。
		path: "/v2/report",
		headers: []string{
			"accept", "authorization", "content-type", "user-agent", "x-domain",
			"x-product", "x-request-id", "x-user-id",
		},
		values: map[string]string{
			"accept":       "application/json, text/plain, */*",
			"content-type": "application/json;charset=UTF-8",
			"x-product":    "SaaS",
			"user-agent":   "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0",
		},
		// 2026-10-30 复核：CN 5.9.3 全部 26 条 /v2/report（含登录前 2 条）**都没有**
		// X-Requested-With。国际版那 10 条带它的样本属于 UA 首段驼峰的渲染器侧另一族，
		// 与本函数发的三段式不是同一个上报通道。
		absent: []string{"origin", "referer", "accept-language", "x-codebuddy-request", "x-device-token", "x-requested-with"},
	},
	{
		// 真机 idx 307/531/380…（19 条）：轻头族，不是 chat 重头族。
		path: "/v2/chat/busy",
		headers: []string{
			"accept", "authorization", "content-type", "user-agent", "x-domain",
			"x-product", "x-request-id", "x-user-id",
		},
		values: map[string]string{
			"accept":       "application/json, text/plain, */*",
			"content-type": "application/json",
			"x-product":    "SaaS",
			"user-agent":   "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0",
		},
		absent: []string{
			"x-codebuddy-request", "x-conversation-id", "x-conversation-request-id",
			"acp-connection-id", "baggage", "b3", "x-requested-with",
			"x-stainless-lang", "origin", "referer", "accept-language",
		},
	},
	{
		// 真机 idx 556。XRW：refresh 的 RPC 族样本（本函数发的这一族）**无**此头，
		// 只有渲染器族 `WorkBuddy/<ver> <slug>/<ver> CLI/…` 那条镜像请求才带
		// （2026-10-30 三条 refresh 逐条核对：RPC 两条无、渲染器一条有）。
		path: "/v2/plugin/auth/token/refresh",
		headers: []string{
			"accept", "authorization", "content-type", "user-agent",
			"x-auth-refresh-source", "x-domain", "x-product", "x-refresh-token",
			"x-request-id", "x-user-id",
		},
		values: map[string]string{
			"accept":                "application/json, text/plain, */*",
			"x-auth-refresh-source": "plugin",
			"x-product":             "SaaS",
			"user-agent":            "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0",
		},
		absent: []string{"origin", "referer", "accept-language", "x-codebuddy-request", "x-requested-with"},
	},
	{
		// 真机 idx 314（23 条 billing 同构）。
		path: "/v2/billing/meter/checkin-activity-status",
		headers: []string{
			"accept", "authorization", "content-type", "user-agent", "x-domain",
			"x-request-id", "x-user-id",
		},
		values: map[string]string{
			"accept":       "application/json",
			"content-type": "application/json",
			"user-agent":   "axios/1.16.1", // billing 层是 axios 实例，与客户端 UA 不同
		},
		absent: []string{"origin", "referer", "accept-language", "x-codebuddy-request"},
	},
	{
		// 真机 idx 558（9 条 /v3/config 同构）。
		// XRW：该路径由 RPC 层打，主流样本（CLI 两段式 UA）**无**此头；带 XRW 的那几条
		// 是渲染器族的镜像请求（UA 为三段 `WorkBuddy/<ver> <slug>/<ver> CLI/…`），
		// 不是本函数这条链。
		path: "/v3/config",
		headers: []string{
			"accept", "authorization", "user-agent", "x-domain", "x-product",
			"x-request-id", "x-user-id",
		},
		values: map[string]string{
			"accept":    "application/json, text/plain, */*",
			"x-product": "SaaS",
			// 2026-10-30 复核：真机 /v3/config 是两段单品牌（同 token 三段/两段返回相同）。
			"user-agent": "WorkBuddy/5.9.3 CLI/2.165.0",
		},
		absent: []string{"origin", "referer", "accept-language", "x-codebuddy-request", "x-requested-with"},
	},
}

// transportOnly 与网关环境相关、不进契约比对的头。
var transportOnly = map[string]bool{
	"host": true, "connection": true, "content-length": true,
	"accept-encoding": true, "x-device-token": true,
	// 客户端按 body 大小才加的压缩头（本测试 body 小，故允许缺省）。
	"content-encoding": true,
}

// TestCNContractGolden 用生产代码真实构造 CN 六条关键路径，逐条比对黄金头集。
func TestCNContractGolden(t *testing.T) {
	captured := map[string]http.Header{}
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" && len(raw) > 2 && raw[0] == 0x1f {
			if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
				raw, _ = io.ReadAll(zr)
			}
		}
		if _, seen := captured[r.URL.Path]; !seen {
			captured[r.URL.Path] = r.Header.Clone()
			order = append(order, r.URL.Path)
		}
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	a := &auth.Auth{
		UID: "00000000-0000-4000-8000-000000000002", AccessToken: "AT",
		RefreshToken: "RT", Nickname: "MisakaNo",
	}
	c := &Client{
		HTTP: srv.Client(), ChatHTTP: srv.Client(),
		ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		GlobalEnabled: true,
	}
	c.SanitizeFingerprints.Store(true)
	c.SanitizeBrandScope.Store("all")

	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	meta := ChatMeta{
		ConversationID:        "a896735e-cc8a-41ae-9a0f-068f43089056",
		ConversationRequestID: "01a1815dc89177bd961af0245527ef7e",
	}
	if rc, _, _, err := c.ChatStream(a, body, "", meta); err == nil && rc != nil {
		io.Copy(io.Discard, rc)
		rc.Close()
	}
	_ = c.ReportChatActivity(a, "e5f5037d-10a2-4889-87ed-1b7539718a64", "01a1815eb602737a9f30ab51740d0ff7")
	_, _ = c.ModelBusy(a, []string{"hy3"})
	_ = c.RefreshToken(a)
	_, _ = c.GetCheckinActivityStatus(a)
	_, _, _ = c.fetchV3ConfigModelMap(a, "")

	for _, g := range cnGoldenCases {
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
			// 集合必须精确相等：多一个（旧实现多发）或少一个（漏发）都是偏差。
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
	_ = order
}

var (
	hex32    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// TestCNIDShapes 会话头族 ID 形态对齐真机：32 hex 时间前缀 / UUIDv7。
func TestCNIDShapes(t *testing.T) {
	hdr := http.Header{}
	req := httptest.NewRequest("POST", "https://www.workbuddy.cn/v2/chat/completions", nil)
	req.Header = hdr
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	// 零值 meta：走「现造」分支，检验兜底形态。
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Conversation-ID"); !uuidV7Re.MatchString(got) {
		t.Errorf("零值 X-Conversation-ID=%q 不是 UUIDv7（真机 01a1815d-dacf-75f1-…）", got)
	}
	if got := req.Header.Get("acp-connection-id"); !uuidV7Re.MatchString(got) {
		t.Errorf("零值 acp-connection-id=%q 不是 UUIDv7", got)
	}
	if got := req.Header.Get("X-Request-ID"); !hex32.MatchString(got) {
		t.Errorf("X-Request-ID=%q 不是 32 hex", got)
	}
	if got := req.Header.Get("X-Conversation-Message-ID"); got != req.Header.Get("X-Request-ID") {
		t.Errorf("消息级两头应同值: %q vs %q", got, req.Header.Get("X-Request-ID"))
	}
	// 真机：X-Trace-ID 与 request id 从不相等（旧实现空则回落 convReqID 是错的）。
	if req.Header.Get("X-Trace-ID") == req.Header.Get("X-Conversation-Request-ID") {
		t.Error("X-Trace-ID 不应等于 X-Conversation-Request-ID")
	}
}
