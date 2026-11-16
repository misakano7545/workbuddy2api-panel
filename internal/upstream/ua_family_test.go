// ua_family_test.go 2026-10-30 双版本真机抓包固化的「UA 分族」与「XRW 归属」结论。
//
// 同一台机器、同一个已登录会话里，官方客户端同时发两种 UA 与两种 XRW 形态，
// 由**哪条代码路径发的**决定，不是按 realm 切：
//
//	               UA                                           XRW
//	chat（SDK）    WorkBuddy/<ver> <slug>/<ver> CLI/<cli>       有
//	RPC/REST       <slug>/<ver> <slug>/<ver> CLI/<cli>          无
//
// 网关两处都发错过一次：RPC 用了 chat 的首段（驼峰），chat 继承了 RPC 的头族。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestRPCUAFirstSegmentIsSlug RPC/REST 族 UA 首段是品牌 slug；国际个人号必须
// 发 `workbuddy-ai/…`，不能发 chat 那种驼峰首段。
func TestRPCUAFirstSegmentIsSlug(t *testing.T) {
	c := &Client{}
	gl := &auth.Auth{UID: "u3", AccessToken: "at", Domain: "www.workbuddy.ai"}
	cn := &auth.Auth{UID: "u1", AccessToken: "at"}

	if got, want := c.rpcUAFor(gl), "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"; got != want {
		t.Errorf("国际 RPC UA=%q want %q", got, want)
	}
	// CN 下 RPC 与 chat 同值（品牌段本来就是驼峰），这是唯一允许相等的组合。
	if got, want := c.rpcUAFor(cn), "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0"; got != want {
		t.Errorf("国内 RPC UA=%q want %q", got, want)
	}

	// 国际版两族必须**不同**：首段 slug vs 驼峰。
	if c.rpcUAFor(gl) == c.userAgent(gl) {
		t.Errorf("国际版 RPC UA 与 chat UA 不应相等，都是 %q", c.rpcUAFor(gl))
	}
	if got, want := c.userAgent(gl), "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"; got != want {
		t.Errorf("国际 chat UA=%q want %q", got, want)
	}
}

// TestXRequestedWithOnlyChat X-Requested-With 只属于 chat 那条链。
//
// 全量抓包（125 条路径）按 UA 族切开后：RPC 族 56/56 无此头，渲染器族 10/10 有，
// chat 5/5 有。网关曾经在 CommonHeaders 里全局注入，等于给 refresh / report /
// busy / console-as 全部多盖一个客户端不发的头。
func TestXRequestedWithOnlyChat(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	chat := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(chat, a, "", ChatMeta{ConversationID: "c1"})
	if got := chat.Header.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("chat X-Requested-With=%q want XMLHttpRequest", got)
	}

	for _, path := range []string{
		"/v2/report", "/v2/chat/busy", "/console/as/conversations/", "/v2/plugin/auth/token/refresh",
	} {
		req := httptest.NewRequest(http.MethodPost, "https://example.invalid"+path, nil)
		c.CommonHeaders(req, a)
		if got := req.Header.Get("X-Requested-With"); got != "" {
			t.Errorf("%s X-Requested-With=%q want 空（RPC 族从不发）", path, got)
		}
	}
}

// TestCommonHeadersRESTShape RPC/REST 公共头族：宽松 Accept + X-Product，无浏览器痕迹。
func TestCommonHeadersRESTShape(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/report", nil)
	c.CommonHeaders(req, a)

	if got, want := req.Header.Get("Accept"), "application/json, text/plain, */*"; got != want {
		t.Errorf("Accept=%q want %q", got, want)
	}
	if got := req.Header.Get("X-Product"); got != "SaaS" {
		t.Errorf("X-Product=%q want SaaS", got)
	}
	for _, h := range []string{"Origin", "Referer", "Accept-Language", "X-CodeBuddy-Request"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("%s=%q want 空（RPC 族从不发）", h, got)
		}
	}
}

// TestChatAcceptIsNarrow chat 只发 application/json（REST 那个宽松值不适用于 SSE 那条链）。
func TestChatAcceptIsNarrow(t *testing.T) {
	c := &Client{}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{ConversationID: "c1"})
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("chat Accept=%q want application/json", got)
	}
}
