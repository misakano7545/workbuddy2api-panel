package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
)

func newTestPanel() *Panel {
	// 启用鉴权：未带 key 的请求一律 401，不进入依赖 Pool/Upstream 的 handler。
	return New(Config{Version: "test", APIKey: "test-key"})
}

// authedGet 带管理密钥 GET（服务端登录页落地后，页面与静态脚本也在默认拒绝门内）。
func authedGet(p *Panel, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+p.adminKey())
	p.ServeHTTP(rec, req)
	return rec
}

// 面板安全响应头必须覆盖：页面、静态脚本、鉴权失败响应。
func TestSecurityHeadersOnAllPanelResponses(t *testing.T) {
	p := newTestPanel()
	paths := []struct{ method, path string }{
		{"GET", "/panel/"},
		{"GET", "/panel/theme.js"},
		{"GET", "/panel/app.js"},
		{"GET", "/panel/api/overview"}, // 401（未提供 key）
		{"POST", "/panel/api/config"},  // 401
		{"GET", "/panel/api/nonexistent"},
	}
	for _, c := range paths {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s %s: missing CSP", c.method, c.path)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: X-Frame-Options=%q", c.method, c.path, h.Get("X-Frame-Options"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, h.Get("Referrer-Policy"))
		}
	}
}

// CSP 必须禁止内联脚本与 iframe 嵌套（严格策略的核心约束）。
func TestCSPDisallowsInlineScriptAndFraming(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	for _, must := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q; got: %s", must, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow unsafe-inline scripts; got: %s", csp)
	}
}

// 页面必须引用外部脚本（内联脚本会被上面的 CSP 拦掉，页面将完全不可用）。
func TestIndexReferencesExternalScript(t *testing.T) {
	p := newTestPanel()
	rec := authedGet(p, "/panel/")
	body := rec.Body.String()

	if !strings.Contains(body, `<script src="app.js?v=`) {
		t.Error("index.html must load app.js externally with content fingerprint (inline script is blocked by CSP)")
	}
	// 反例保护：出现内联 <script>...</script> 内容块即为回归
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html still contains an inline <script> block; CSP would block it")
	}
}

// app.js 必须能作为同源脚本取到且类型正确（否则页面白屏）。
func TestAppScriptServed(t *testing.T) {
	p := newTestPanel()
	rec := authedGet(p, "/panel/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type=%q want javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "'use strict'") {
		t.Error("app.js body looks wrong")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control=%q want no-store", rec.Header().Get("Cache-Control"))
	}
}

// 页面里的脚本 URL 必须带内容指纹，且指纹与真发的脚本内容对得上（issue #33）。
// 钉的是：URL 查询串里的 v= 就是所服务脚本的 sha256 前 8 字节——指纹算错/
// 接错内容等于没防，这条断言把它钉死。
func TestScriptURLsCarryContentFingerprint(t *testing.T) {
	p := newTestPanel()

	page := authedGet(p, "/panel/")
	body := page.Body.String()

	appRec := authedGet(p, "/panel/app.js")
	themeRec := authedGet(p, "/panel/theme.js")

	for _, c := range []struct {
		name string
		src  string
		data string
	}{
		{"app.js", `src="app.js?v=`, appRec.Body.String()},
		{"theme.js", `src="theme.js?v=`, themeRec.Body.String()},
	} {
		i := strings.Index(body, c.src)
		if i < 0 {
			t.Fatalf("%s 引用未带指纹: %s", c.name, c.src)
		}
		rest := body[i+len(c.src):]
		j := strings.IndexByte(rest, '"')
		if j < 8 {
			t.Fatalf("%s 指纹形态异常: %q", c.name, rest[:min(j, len(rest))])
		}
		sum := sha256.Sum256([]byte(c.data))
		want := hex.EncodeToString(sum[:8])
		if got := rest[:j]; got != want {
			t.Errorf("%s URL 指纹=%s 内容实算=%s（两者必须一致）", c.name, got, want)
		}
	}
}

// 首屏主题预热脚本：必须能同源取到，且引用的位置早于样式表。
// 顺序是这个问题唯一的关键——放到 <body> 或 <style> 之后，就会退回到
// 「刷新先按默认深色画一帧，再翻成用户选的主题」的老毛病（用户可见的闪烁）。
func TestThemeBootScriptRunsBeforeStyles(t *testing.T) {
	p := newTestPanel()

	rec := authedGet(p, "/panel/theme.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("theme.js code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("theme.js Content-Type=%q want javascript", ct)
	}
	// 键名与 app.js 的 LS_THEME 必须一致，否则预热读不到用户偏好、白闪照旧。
	if !strings.Contains(rec.Body.String(), "wb2api.theme") {
		t.Error("theme.js 未读取 wb2api.theme，与 app.js 的存储键对不上")
	}

	page := authedGet(p, "/panel/")
	body := page.Body.String()
	iScript := strings.Index(body, `<script src="theme.js?v=`)
	if iScript < 0 {
		t.Fatal("index.html 未引用 theme.js")
	}
	iStyle := strings.Index(body, "<style>")
	if iStyle < 0 || iScript > iStyle {
		t.Errorf("theme.js 必须早于 <style> 加载: script@%d style@%d", iScript, iStyle)
	}
}

// UID 白名单：拒绝路径穿越与异常字符，放行真实 UUID 形态。
func TestValidUID(t *testing.T) {
	ok := []string{
		"248890d9-bb26-4131-87a7-4ec74d472344",
		"abc_123-XYZ",
		"a",
	}
	bad := []string{
		"",
		"../../evil",
		"x/../../y",
		`..\..\evil`,
		"a/b",
		"a\\b",
		"uid with space",
		"uid\nnewline",
		"uid\x00null",
		"café",
		strings.Repeat("a", 65), // 超长
	}
	for _, u := range ok {
		if !validUID(u) {
			t.Errorf("validUID(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validUID(u) {
			t.Errorf("validUID(%q) = true, want false", u)
		}
	}
}

// 未带密钥的 API 请求必须 401；携带正确密钥则通过鉴权层（不再是 401）。
func TestAuthLayerBehavior(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d want 401", rec.Code)
	}
	// 用不存在的路由验证"带正确 key 已过鉴权"（避免触碰依赖 nil 的 handler）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/nonexistent", nil)
	req2.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Error("valid key must pass the auth layer")
	}
}

// ---------------------------------------------------------------------------
// 管理端会话 / 默认拒绝门 / 登录限速
// ---------------------------------------------------------------------------

// 会话令牌必须绑定密钥与过期时刻：换密钥、改过期、过期本身都要验不过。
func TestAdminSessionTokenRoundTrip(t *testing.T) {
	key := "admin-secret"
	now := time.Unix(1_700_000_000, 0)
	exp := now.Add(time.Hour)
	tok := adminSessionToken(key, exp)

	if !adminSessionOK(key, tok, now) {
		t.Fatal("合法令牌被拒")
	}
	if adminSessionOK("another-key", tok, now) {
		t.Error("换密钥后旧令牌必须失效")
	}
	if adminSessionOK(key, tok, exp) {
		t.Error("过期时刻（含等于）必须拒绝")
	}
	// 篡改过期时刻、保留原签名：签名取自原 payload，改了就验不过。
	dot := strings.IndexByte(tok, '.')
	tampered := strconv.FormatInt(exp.Add(time.Hour).Unix(), 10) + tok[dot:]
	if adminSessionOK(key, tampered, now) {
		t.Error("篡改过期时刻必须验签失败")
	}
	for _, bad := range []string{"", ".", "abc.def", tok + "x", "x" + tok} {
		if adminSessionOK(key, bad, now) {
			t.Errorf("畸形令牌 %q 必须拒绝", bad)
		}
	}
}

// next 参数只放行站内 /panel/ 路径（登录页落地时最容易引入开放重定向）。
func TestSanitizeNextRejectsOpenRedirect(t *testing.T) {
	ok := map[string]string{
		"/panel/":        "/panel/",
		"/panel/usage":   "/panel/usage",
		"/panel/?x=1#y":  "/panel/?x=1#y",
		"/panel/models/": "/panel/models/",
	}
	for in, want := range ok {
		if got := sanitizeNext(in); got != want {
			t.Errorf("sanitizeNext(%q)=%q want %q", in, got, want)
		}
	}
	bad := []string{
		"",
		"https://evil.example/panel/",
		"//evil.example/panel/",
		`/\evil.example`,
		"/panel/login",
		"/panel/login?next=/panel/",
		"/panel\\evil",
		"/panel/\r\nLocation: https://evil.example",
		"/etc/passwd",
		"javascript:alert(1)",
	}
	for _, in := range bad {
		if got := sanitizeNext(in); got != "/panel/" {
			t.Errorf("sanitizeNext(%q)=%q，必须回落 /panel/", in, got)
		}
	}
}

// 固定窗口限速：第 11 次拒绝、其它 IP 不受影响、窗口过后恢复、成功后 reset。
func TestLoginThrottleFixedWindow(t *testing.T) {
	var th loginThrottle
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < adminLoginMaxAttempts; i++ {
		if ok, _ := th.take("203.0.113.9", now); !ok {
			t.Fatalf("第 %d 次尝试不该被拒", i+1)
		}
	}
	ok, wait := th.take("203.0.113.9", now)
	if ok {
		t.Fatal("超过上限必须拒绝")
	}
	if wait <= 0 || wait > adminLoginWindow {
		t.Errorf("Retry-After 剩余窗口=%v，应在 (0, %v]", wait, adminLoginWindow)
	}
	if ok, _ := th.take("203.0.113.10", now); !ok {
		t.Error("限速必须按来源 IP 分桶，不能连坐")
	}
	// 窗口滚过：重新计数。
	if ok, _ := th.take("203.0.113.9", now.Add(adminLoginWindow)); !ok {
		t.Error("固定窗口过期后应恢复")
	}
	th.reset("203.0.113.9")
	if ok, _ := th.take("203.0.113.9", now.Add(adminLoginWindow)); !ok {
		t.Error("reset 后应恢复")
	}
}

// 默认拒绝门：页面/脚本/未知路由匿名一律拦截，登录页与脚本白名单放行。
func TestPanelDefaultDenyAndLoginPage(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "gw-key", AdminKey: "ops-key"})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("匿名页面 code=%d want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/panel/login?next=") {
		t.Errorf("匿名页面应跳登录页并带 next，got %q", loc)
	}

	// 未知 API 路由匿名也必须是 401，而不是「公开 404」——新增路由默认要鉴权。
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/brand_new_route", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未知路由匿名 code=%d want 401（默认拒绝）", rec.Code)
	}

	// 登录页与脚本在白名单内（登录前必须可取到）。
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/login?next=/panel/usage", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("登录页 code=%d want 200", rec.Code)
	}
	for _, must := range []string{`src="login.js?v=`, `id="key"`, `id="go"`} {
		if !strings.Contains(body, must) {
			t.Errorf("登录页缺少 %q（外链脚本才符合 script-src 'self'）", must)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("登录页不得内联脚本（CSP 会拦掉）")
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/login.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/panel/api/login") {
		t.Errorf("login.js code=%d，需含登录提交逻辑", rec.Code)
	}

	// 网关密钥不能进后台（两把钥匙必须真的分开）。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/", nil)
	req.Header.Set("Authorization", "Bearer gw-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Error("网关 api_key 不得通过管理端鉴权")
	}
}

// 登录成功下发 HttpOnly 会话 Cookie；错密钥 401 且不下发；Cookie 可换页面访问。
func TestAdminLoginSetsHttpOnlySessionCookie(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "gw-key", AdminKey: "ops-key"})

	post := func(key, next string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/panel/api/login",
			strings.NewReader(`{"key":"`+key+`","next":"`+next+`"}`))
		req.Header.Set("Content-Type", "application/json")
		p.ServeHTTP(rec, req)
		return rec
	}

	bad := post("wrong", "/panel/")
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("错密钥 code=%d want 401", bad.Code)
	}
	if len(bad.Result().Cookies()) != 0 {
		t.Error("登录失败不得下发会话 Cookie")
	}

	good := post("ops-key", "/panel/usage")
	if good.Code != http.StatusOK {
		t.Fatalf("正确密钥 code=%d want 200: %s", good.Code, good.Body.String())
	}
	if !strings.Contains(good.Body.String(), `"/panel/usage"`) {
		t.Errorf("登录响应需回传 next，got %s", good.Body.String())
	}
	var sess *http.Cookie
	for _, c := range good.Result().Cookies() {
		if c.Name == adminCookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("登录成功必须下发会话 Cookie")
	}
	if !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/panel" || sess.Value == "" {
		t.Errorf("Cookie 属性不安全: %+v", sess)
	}

	// 带会话 Cookie 访问页面 → 200（浏览器里不再需要明文密钥）。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: sess.Value})
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("携带会话 Cookie 的页面 code=%d want 200", rec.Code)
	}

	// 伪造 Cookie 不通过。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/panel/", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: sess.Value + "x"})
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Error("篡改过的会话 Cookie 必须被拒")
	}
}

// 登录失败限速：同一来源第 11 次返回 429 + Retry-After。
func TestAdminLoginRateLimited(t *testing.T) {
	p := New(Config{Version: "test", AdminKey: "ops-key"})
	var last *httptest.ResponseRecorder
	for i := 0; i < adminLoginMaxAttempts+1; i++ {
		last = httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/panel/api/login", strings.NewReader(`{"key":"nope"}`))
		p.ServeHTTP(last, req)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次尝试 code=%d want 429", adminLoginMaxAttempts+1, last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("429 必须带 Retry-After")
	}
}

// 未配置 admin_key 时回落 api_key（升级零感知），并透出 admin_key_shared。
func TestAdminKeyFallsBackToGatewayKey(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "gw-key"})
	if p.adminKey() != "gw-key" {
		t.Errorf("adminKey=%q want 回落 gw-key", p.adminKey())
	}
	if !p.adminKeyShared() {
		t.Error("未分离时必须标记 admin_key_shared")
	}
	// 升级兼容：老配置仍用 api_key Bearer 进页面。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/", nil)
	req.Header.Set("Authorization", "Bearer gw-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("回落模式下 api_key 应可进面板，code=%d", rec.Code)
	}

	sep := New(Config{Version: "test", APIKey: "gw-key", AdminKey: "ops-key"})
	if sep.adminKey() != "ops-key" || sep.adminKeyShared() {
		t.Errorf("独立 admin_key 生效失败: key=%q shared=%v", sep.adminKey(), sep.adminKeyShared())
	}
}

// 热更新语义：live 快照永远压过启动期静态字段；在面板里清空 admin_key 后
// 必须真的回落到网关密钥，而不是被启动值续命。
func TestAdminKeyLiveSnapshotWins(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "gw-key", AdminKey: "live-ops"})
	p := New(Config{Version: "test", APIKey: "gw-key", AdminKey: "startup-ops", Live: live})
	if got := p.adminKey(); got != "live-ops" {
		t.Errorf("adminKey=%q want live-ops（快照优先）", got)
	}
	live.Store(livecfg.Snapshot{APIKey: "gw-key"})
	if got := p.adminKey(); got != "gw-key" {
		t.Errorf("清空 admin_key 后 adminKey=%q want 回落 gw-key（不能是启动值）", got)
	}
	if !p.adminKeyShared() {
		t.Error("清空后应重新标记 admin_key_shared")
	}
}
