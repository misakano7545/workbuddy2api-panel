// fingerprint_test.go 官方客户端指纹对齐真机（国内 5.9.3 / 国际 5.8.4）的回归。
//
// 证据来源：2026-10-20 桌面客户端真实启动流量（mitmproxy 抓到 www.workbuddy.ai
// 183 条请求）+ 客户端 app.asar / cli/dist 静态反编译。以下每个断言都对应一处
// 与真机不一致的地方——不一致本身是「被判定非官方客户端」的候选指纹。
package upstream

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

func globalAuth() *auth.Auth {
	return &auth.Auth{UID: "u-g", AccessToken: "at", Domain: "www.workbuddy.ai", Nickname: "g"}
}

// v3ProbeUACNForTest 国内版 /v3/config 实测 UA（默认 Client、CN 账号）。
//
// 2026-10-30 国内 5.9.3 抓包复核：真机 /v3/config 是**两段式**
// （版本段跟当前分发包，现 5.11.3 / CLI 2.176.0）。同一账号同一 token 用三段 / 两段分别打，
// 返回逐字节相同的 64866 字节（61 模型 / agents=cli:16）——UA 家族不再分流。
const v3ProbeUACNForTest = "WorkBuddy/5.11.3 CLI/2.176.0"

// TestUARealmBrand 首段恒驼峰 WorkBuddy，只有第二段品牌随 realm 变。
//
// 2026-10-30 双版本真机抓包（chat 链 /v2/chat/completions）：
//
//	CN 5.11.3    → WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0
//	global 5.9.4 → WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
//
// 关键：首段永远是驼峰 `WorkBuddy`——官方 chat 走 OpenAI SDK 的 buildUserAgent，
// 首段取 productName 原样，第二段才是打包期改写的 slug。曾把 global 首段也写成
// `workbuddy-ai/`（与 /v2/report 那类非 SDK 请求混淆），与 chat 真机不符。
func TestUARealmBrand(t *testing.T) {
	c := New()

	cn := &auth.Auth{UID: "u-c", AccessToken: "at"}
	if got, want := c.userAgent(cn), "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0"; got != want {
		t.Errorf("CN UA=%q want %q", got, want)
	}
	if got, want := c.userAgent(globalAuth()), "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"; got != want {
		t.Errorf("global UA=%q want %q", got, want)
	}
	// 显式配置仍然赢（用户自定义品牌/版本）。
	c.ClientVersion = "9.9.9"
	c.CliVersion = "1.2.3"
	if got, want := c.userAgent(globalAuth()), "WorkBuddy/9.9.9 workbuddy-ai/9.9.9 CLI/1.2.3"; got != want {
		t.Errorf("override UA=%q want %q", got, want)
	}
}

// TestV3ProbeUAOfficial 目录探测 UA 必须是官方真实形态，不能是 CodeBuddy 家族。
//
// 2026-10-30 双版本真机 /v3/config UA **同一形态**（两段、单品牌）：
//
//	CN 5.9.3     → WorkBuddy/5.11.3 CLI/2.176.0
//	global 5.9.4 → WorkBuddy/5.9.4 CLI/2.165.0
//
// 旧实现给 CN 发三段重复品牌（5.8.3 时代的抓包结论），5.9.3 已不成立：同 token
// 三段/两段返回逐字节相同。更早的 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` /
// `CLI/2.63.2 CodeBuddy/2.63.2` 两个串在 WorkBuddy 分发包里根本不存在。
func TestV3ProbeUAOfficial(t *testing.T) {
	c := New()
	cn := &auth.Auth{UID: "u-c", AccessToken: "at"}

	if got, want := c.v3ProbeUA(cn), "WorkBuddy/5.11.3 CLI/2.176.0"; got != want {
		t.Errorf("CN v3ProbeUA=%q want %q", got, want)
	}
	if got, want := c.v3ProbeUA(globalAuth()), "WorkBuddy/5.9.4 CLI/2.165.0"; got != want {
		t.Errorf("global v3ProbeUA=%q want %q", got, want)
	}
	// 第二路与主路同串（真机只有这一种形态；双路只为沿用 global 的并集调用链）。
	if got, want := c.v3ProbeUALite(cn), "WorkBuddy/5.11.3 CLI/2.176.0"; got != want {
		t.Errorf("CN v3ProbeUALite=%q want %q", got, want)
	}
	// 任何一路都不许再出现 CodeBuddy 字面量。
	for _, ua := range []string{c.v3ProbeUA(cn), c.v3ProbeUA(globalAuth()), c.v3ProbeUALite(cn), c.desktopProbeUA()} {
		if strings.Contains(ua, "CodeBuddy") {
			t.Errorf("探测 UA 仍含 CodeBuddy 家族字面量: %q", ua)
		}
	}
}

// TestBillingUAAxiosBothRealms 两版计费请求的 UA 都是 axios 默认串。
//
// 真机实测：CN 与 GL 的 `/billing/meter/get-user-resource-*`、`-summary`、
// `checkin-activity-status` 的 UA 全是 `axios/1.16.1` —— billing 层走 axios 实例，
// 不带任何品牌段。旧实现 CN 侧发 `WorkBuddy/<ver>` 是按客户端 UA 推理的产物。
func TestBillingUAAxiosBothRealms(t *testing.T) {
	c := New()
	if got := c.billingUA(globalAuth()); got != "axios/1.16.1" {
		t.Errorf("global billing UA=%q want axios/1.16.1", got)
	}
	if got := c.billingUA(&auth.Auth{UID: "u-c", AccessToken: "at"}); got != "axios/1.16.1" {
		t.Errorf("CN billing UA=%q want axios/1.16.1", got)
	}
}

// TestIdentityHeadersPersonalSendsNone 个人号三头全不发。
//
// 反编译的 runIdentityHeaders() 只在「有 enterpriseId / idSource / token_source 任一」
// 时才发 X-Userinfo；真机个人号抓包里 X-Userinfo / X-Auth-Method / X-Id-Source 全都不存在。
// 凭「个人号也该有」去补空值是伪造指纹，比不发更容易被识别。
func TestIdentityHeadersPersonalSendsNone(t *testing.T) {
	c := New()
	a := globalAuth()
	req := httptest.NewRequest("POST", "https://www.workbuddy.ai/v2/chat/completions", nil)
	c.injectIdentityHeaders(req, a)
	for _, h := range []string{"X-Userinfo", "X-Auth-Method", "X-Id-Source"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("个人号不该发 %s=%q", h, v)
		}
	}
}

// TestIdentityHeadersEnterpriseUserinfo 企业号发 X-Userinfo，内容是 base64(JSON{uin,owner_uin})。
// uin 段取 uid（反编译逻辑是 {uin: n.uid}），owner_uin 取 enterpriseId。
func TestIdentityHeadersEnterpriseUserinfo(t *testing.T) {
	c := New()
	a := globalAuth()
	a.EnterpriseID = "ent-1"
	req := httptest.NewRequest("POST", "https://www.workbuddy.ai/v2/chat/completions", nil)
	c.injectIdentityHeaders(req, a)

	raw := req.Header.Get("X-Userinfo")
	if raw == "" {
		t.Fatal("企业号应发 X-Userinfo")
	}
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("X-Userinfo 不是标准 base64: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(dec, &payload); err != nil {
		t.Fatalf("X-Userinfo 不是 JSON: %v (%s)", err, dec)
	}
	if payload["uin"] != "u-g" {
		t.Errorf("uin=%q want u-g（反编译逻辑是 {uin: uid}）", payload["uin"])
	}
	if payload["owner_uin"] != "ent-1" {
		t.Errorf("owner_uin=%q want ent-1", payload["owner_uin"])
	}
	if _, ok := payload["token_source"]; ok {
		t.Errorf("无 token_source 时不该写入该键（不是写空串）：%v", payload)
	}
}

// TestIdentityHeadersPersonalWithTokenSourceSendsNone 个人号即使 token 带
// token_source 也不发 X-Auth-Method / X-Userinfo（issue #51）。
//
// 背景：`login.sh` 的设备流拿到的 CN token 声明里有 token_source=enterprise_switch，
// 旧逻辑据此给个人号注入了这两个头；而两版客户端的个人号真机抓包都没有它们
// （capture/raw3/001-chat.json 国际版、capture/raw8/002-chat.json 国内版）。
// token_source 是 token 的来路，不是账号身份——个人号 enterpriseId 为空即无企业语义。
func TestIdentityHeadersPersonalWithTokenSourceSendsNone(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`))
	a := &auth.Auth{UID: "u1", AccessToken: "h." + payload + ".s"} // 无 EnterpriseID = 个人号
	req := httptest.NewRequest("POST", "https://example/v2/chat/completions", nil)
	New().injectIdentityHeaders(req, a)

	for _, h := range []string{"X-Auth-Method", "X-Userinfo", "X-Id-Source"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("个人号不该发 %s=%q（官方两版个人号都没有这个头）", h, v)
		}
	}
}

// TestIdentityHeadersEnterpriseWithTokenSource 企业号仍按 token_source 注入。
func TestIdentityHeadersEnterpriseWithTokenSource(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`))
	a := &auth.Auth{UID: "u1", EnterpriseID: "ent-1", AccessToken: "h." + payload + ".s"}
	req := httptest.NewRequest("POST", "https://example/v2/chat/completions", nil)
	New().injectIdentityHeaders(req, a)

	if got := req.Header.Get("X-Auth-Method"); got != "enterprise_switch" {
		t.Fatalf("企业号 X-Auth-Method=%q want enterprise_switch", got)
	}
	dec, _ := base64.StdEncoding.DecodeString(req.Header.Get("X-Userinfo"))
	var m map[string]string
	_ = json.Unmarshal(dec, &m)
	if m["token_source"] != "enterprise_switch" || m["uin"] != "u1" || m["owner_uin"] != "ent-1" {
		t.Fatalf("X-Userinfo=%s", dec)
	}
}

// TestChatCarriesPrivateData 真机 chat 出站恒带 X-Private-Data: **false**。
//
// 5.9.3 真机 7 条 /v2/chat/completions 一律 `X-Private-Data: false`（含 main 与
// auxiliary）。客户端语义是「开了模型优化 → 声明 false」，只有企业版/显式关优化才发 true。
func TestChatCarriesPrivateData(t *testing.T) {
	c := New()
	c.ChatBaseCN = "https://cn.example"
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req := httptest.NewRequest("POST", "https://cn.example/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{ConversationID: "c1", ConversationRequestID: "r1"})
	if got := req.Header.Get("X-Private-Data"); got != "false" {
		t.Fatalf("X-Private-Data=%q want false", got)
	}
	// 同批真机头里必带 X-Agent-Runtime: local-agent。
	if got := req.Header.Get("X-Agent-Runtime"); got != "local-agent" {
		t.Fatalf("X-Agent-Runtime=%q want local-agent", got)
	}
	// baggage 必须与同请求的会话头同值（头体/头间一致性）。
	if got := req.Header.Get("baggage"); !strings.Contains(got, "codebuddy.session_id=c1") ||
		!strings.Contains(got, "codebuddy.conversation_request_id=r1") {
		t.Fatalf("baggage=%q 需含 session/conversation_request 同值", got)
	}
}

// TestDesktopReportDomainIsHost X-Domain 必须是裸主机名，不是带 scheme 的 URL。
// 真机：`X-Domain: www.workbuddy.ai`；原实现塞的是 desktopBase（https://...）。
func TestDesktopReportDomainIsHost(t *testing.T) {
	var gotDomain string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDomain = r.Header.Get("X-Domain")
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	// 用真 base 形式（带 scheme）验证剥离逻辑：client 的 base 就是 URL。
	c := &Client{HTTP: srv.Client(), ChatBaseGlobal: srv.URL, GlobalEnabled: true}
	if err := c.ReportDesktopEvent(globalAuth(), DesktopEvent{"eventCode": "x"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	// Hostname() 剥掉 scheme 与端口（真机 X-Domain 是纯主机名，无端口）。
	u, _ := url.Parse(srv.URL)
	want := u.Hostname()
	if gotDomain != want {
		t.Fatalf("X-Domain=%q want %q（裸 host，不带 scheme）", gotDomain, want)
	}
}

// TestDesktopReportFingerprintPerRealm /v2/report 的版本三元组按 realm 分档，
// 且 qimei36 只在国内版出现。
//
// 2026-10-20 双版本真机抓包（同一台机器）：
//
//	CN     ideVersion 5.11.3 commit ecace0f7… releaseDate 1794502169764，**带** qimei36
//	global ideVersion 5.8.4 commit 73952edb48da… releaseDate 1792063084107，**无** qimei36
//
// 键数差集恰好只有 qimei36（CN 28 键 / GL 27 键）。此前不分档、恒用 GL 的三元组，
// 等于给每个国内号都盖了国际版的包指纹；给 GL 补 qimei36 则是无中生有。
func TestDesktopReportFingerprintPerRealm(t *testing.T) {
	cases := []struct {
		name    string
		auth    *auth.Auth
		ver     string
		commit  string
		release int64
		qimei   bool
	}{
		{"CN", &auth.Auth{UID: "u-c", AccessToken: "at", Nickname: "cn"},
			"5.11.3", "ecace0f7028166695994300182295329b8b95449", 1794502169764, true},
		{"global", globalAuth(),
			"5.9.4", "fa0a0901906addaa1a4259f6df347388a5563ded", 1793288804029, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := desktopFingerprint(tc.auth)
			if fp["ideVersion"] != tc.ver || fp["extVersion"] != tc.ver {
				t.Errorf("ideVersion/extVersion=%v/%v want %s", fp["ideVersion"], fp["extVersion"], tc.ver)
			}
			if fp["commit"] != tc.commit {
				t.Errorf("commit=%v want %s", fp["commit"], tc.commit)
			}
			if fp["releaseDate"] != tc.release {
				t.Errorf("releaseDate=%v want %d", fp["releaseDate"], tc.release)
			}
			got, has := fp["qimei36"]
			if has != tc.qimei {
				t.Fatalf("qimei36 存在性=%v want %v（CN 有 / global 无）", has, tc.qimei)
			}
			if tc.qimei {
				s, _ := got.(string)
				if len(s) != 36 {
					t.Errorf("qimei36=%q 长度=%d want 36 hex", s, len(s))
				}
				// 同号幂等：设备标识跨调用必须恒同值。
				if again := desktopFingerprint(tc.auth)["qimei36"]; again != got {
					t.Errorf("qimei36 非幂等: %v vs %v", got, again)
				}
			}
		})
	}
}

// TestDesktopUAPerRealm 桌面端 UA 的品牌段与客户端版本都跟着 realm 走。
func TestDesktopUAPerRealm(t *testing.T) {
	cn := &auth.Auth{UID: "u-c", AccessToken: "at"}
	if got, want := desktopUAFor(cn), "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0"; got != want {
		t.Errorf("CN desktop UA=%q want %q", got, want)
	}
	if got, want := desktopUAFor(globalAuth()), "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"; got != want {
		t.Errorf("global desktop UA=%q want %q", got, want)
	}
	// RPC 层 UA 与 desktop 同形（首段即品牌 slug），chat 层则由 userAgent 覆盖。
	if got, want := (&Client{}).rpcUAFor(globalAuth()), "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"; got != want {
		t.Errorf("global rpc UA=%q want %q", got, want)
	}
	if got, want := (&Client{}).rpcUAFor(cn), "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0"; got != want {
		t.Errorf("CN rpc UA=%q want %q（CN 下与 chat 同值）", got, want)
	}
}

// TestAcceptLanguageSameBothRealms Accept-Language 不按账号域切。
//
// 真机实测：跟随操作系统区域，同一台中文机器上 CN/GL 都是 `zh`（billing 层固定）
// 或 `zh-CN`（渲染层），多数请求干脆不带。旧实现「global → en-US」是臆测。
func TestAcceptLanguageSameBothRealms(t *testing.T) {
	cn, gl := acceptLanguageFor(&auth.Auth{UID: "u-c"}), acceptLanguageFor(globalAuth())
	if cn != gl {
		t.Errorf("两域取值不一致: CN=%q GL=%q（真机同机同值）", cn, gl)
	}
	if cn == "en-US" {
		t.Errorf("不应按域硬编码 en-US（实测仅出现 1 次，非常态）")
	}
}

// TestGlobalPackageDetailsBodyShapes paid / free 两份明细的请求体**不同形**。
//
// 2026-10-20 双版本真机抓包（CN/GL 一致）：
//
//	paid-packages: {PageNumber, PageSize, PackageCodes, Status:[0,3], NeedRenewInfo:true}
//	               —— 无 SlicePeriod 时间切片
//	free-packages: … + SlicePeriodStartTime / SlicePeriodEndTime
//	               —— 无 NeedRenewInfo
//
// 此前两条共用同一 body（都带切片、都无 NeedRenewInfo），paid 包因此拿不到续订窗口。
func TestGlobalPackageDetailsBodyShapes(t *testing.T) {
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		bodies[name] = m
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[]}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	c.globalPackageDetails(globalAuth(), []string{"PKG-A"})

	paid, ok := bodies["get-user-resource-paid-packages"]
	if !ok {
		t.Fatalf("未请求 paid-packages，实际=%v", bodies)
	}
	if _, has := paid["SlicePeriodStartTime"]; has {
		t.Errorf("paid-packages 不该带时间切片: %v", paid)
	}
	if paid["NeedRenewInfo"] != true {
		t.Errorf("paid-packages 必须带 NeedRenewInfo:true，实际=%v", paid["NeedRenewInfo"])
	}

	free, ok := bodies["get-user-resource-free-packages"]
	if !ok {
		t.Fatalf("未请求 free-packages，实际=%v", bodies)
	}
	if _, has := free["NeedRenewInfo"]; has {
		t.Errorf("free-packages 不该带 NeedRenewInfo: %v", free)
	}
	if free["SlicePeriodStartTime"] == nil || free["SlicePeriodEndTime"] == nil {
		t.Errorf("free-packages 必须带时间切片: %v", free)
	}

	// 两条共有字段必须一致（差集只允许上面那两处）。
	for _, k := range []string{"PageNumber", "PageSize", "PackageCodes", "Status"} {
		if paid[k] == nil || free[k] == nil {
			t.Errorf("共有字段 %s 缺失: paid=%v free=%v", k, paid[k], free[k])
		}
	}
}

// TestGlobalBillingSummary 国际版余额走 summary + paid/free 明细：
// remain/total 取 summary 的 Cycle{Remain,Total}Capacity（字符串小数），
// 到期批次按 PackageCode 与明细关联。
func TestGlobalBillingSummary(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Packages":[` +
				`{"PackageCode":"PKG-A","CycleTotalCapacity":"540","CycleRemainCapacity":"335.47000026","CycleUsedCapacity":"204.52999974","CapacityUnit":"credit"},` +
				`{"PackageCode":"PKG-B","CycleTotalCapacity":"100","CycleRemainCapacity":"0","CycleUsedCapacity":"100"}` +
				`],"SubscriptionPackageName":"免费版","IsPaidUser":false}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-free-packages"):
			soon := time.Now().Add(48 * time.Hour).Format("2006-01-02 15:04:05")
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[` +
				`{"PackageCode":"PKG-A","PackageName":"权益赠送包","CycleCapacitySize":540,"CycleCapacityRemain":335,` +
				`"CycleEndTime":"` + soon + `","DeductionEndTime":0,"ExpiredTime":""}` +
				`],"TotalCount":1}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-paid-packages"):
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[],"TotalCount":0}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err :=
		c.UserResourceSnapshot(globalAuth(), 168*time.Hour)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// 540 + 100 = 640 total；335.47 + 0 → 截断 335 remain。
	if total != 640 {
		t.Errorf("total=%d want 640", total)
	}
	if remain != 335 {
		t.Errorf("remain=%d want 335（字符串小数按整数截断，与既有口径一致）", remain)
	}
	if len(buckets) != 1 || buckets[0].Remain != 335 {
		t.Fatalf("buckets=%+v want 单批 335", buckets)
	}
	if earliestAt.IsZero() || earliestRemaining != 335 {
		t.Errorf("earliestAt=%v earliestRemaining=%d want 非零/335", earliestAt, earliestRemaining)
	}
	// 48h 后到期、窗口 168h → 该批整笔计入 expiring。
	if expiring != 335 {
		t.Errorf("expiring=%d want 335（48h < 168h 窗口）", expiring)
	}
	// 必须打的是国际版新三件套，不能退回 CN 的 get-user-resource。
	for _, p := range paths {
		if strings.HasSuffix(p, "/billing/meter/get-user-resource") {
			t.Fatalf("国际版不该再走 CN 分页路径：%v", paths)
		}
	}
}

// TestGlobalBillingFallsBackToCNPath summary 失败时回落 CN 分页路径（老路径兼容尝试）。
func TestGlobalBillingFallsBackToCNPath(t *testing.T) {
	var sawCN bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-user-resource-summary") {
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "get-user-resource") &&
			!strings.Contains(r.URL.Path, "paid-packages") &&
			!strings.Contains(r.URL.Path, "free-packages") {
			sawCN = true
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":` +
				`[{"CycleCapacitySize":1000,"CycleCapacityRemain":900,"CycleCapacityUsed":100}]}}}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	remain, total, _, _, _, _, err := c.UserResourceSnapshot(globalAuth(), 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !sawCN {
		t.Fatal("summary 失败时应回落到 CN 分页路径")
	}
	if remain != 900 || total != 1000 {
		t.Errorf("remain=%d total=%d want 900/1000", remain, total)
	}
}
