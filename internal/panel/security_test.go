package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/httpauth"
)

func newTestPanel() *Panel {
	// 启用鉴权：未带 key 的请求一律 401，不进入依赖 Pool/Upstream 的 handler。
	return New(Config{Version: "test", APIKey: "test-key"})
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
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
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
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
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

	page := httptest.NewRecorder()
	p.ServeHTTP(page, httptest.NewRequest("GET", "/panel/", nil))
	body := page.Body.String()

	appRec := httptest.NewRecorder()
	p.ServeHTTP(appRec, httptest.NewRequest("GET", "/panel/app.js", nil))
	themeRec := httptest.NewRecorder()
	p.ServeHTTP(themeRec, httptest.NewRequest("GET", "/panel/theme.js", nil))

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

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/theme.js", nil))
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

	page := httptest.NewRecorder()
	p.ServeHTTP(page, httptest.NewRequest("GET", "/panel/", nil))
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
// 命名推理 key：面板只做管理，且只认主口令
// ---------------------------------------------------------------------------

func testKeysStore(t *testing.T) *httpauth.Store {
	t.Helper()
	s, err := httpauth.NewKeyStore(filepath.Join(t.TempDir(), "api_keys.json"))
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	return s
}

// TestPanelRejectsNamedKey 推理面的命名 key 绝不能顺带开进运维面。
func TestPanelRejectsNamedKey(t *testing.T) {
	store := testKeysStore(t)
	_, secret, err := store.Create("ci", httpauth.Policy{}, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := New(Config{Version: "test", APIKey: "master", Keys: store})

	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("命名 key 不该进面板，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestPanelClosedWhenKeysConfiguredWithoutMaster 空主口令 + 非空 key 表：匿名进不了面板。
func TestPanelClosedWhenKeysConfiguredWithoutMaster(t *testing.T) {
	store := testKeysStore(t)
	if _, _, err := store.Create("ci", httpauth.Policy{}, time.Time{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	p := New(Config{Version: "test", Keys: store})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("配了 key 表时匿名应 401，got %d body=%s", rec.Code, rec.Body)
	}

	// 没有 key 表时维持老的「不鉴权」语义（依赖 Pool 的 handler 会 5xx/501，
	// 但不该是 401）。
	open := New(Config{Version: "test"})
	rec = httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/keys", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("无 key 表且空主口令时应维持不鉴权")
	}
}

// TestPanelKeyCRUD 管理端点：新建只回一次明文、列表不含摘要、轮换与删除生效。
func TestPanelKeyCRUD(t *testing.T) {
	store := testKeysStore(t)
	p := New(Config{Version: "test", APIKey: "master", Keys: store})
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer master")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	rec := call("POST", "/panel/api/keys", `{"name":"ci","policy":{"rpm":5,"max_concurrent":2,"daily_credits":1.5,"models":["glm-5.2"]}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "wb2a_") {
		t.Fatalf("新建应返回一次性 secret，got %d body=%s", rec.Code, rec.Body)
	}
	var created struct {
		Key    httpauth.View `json:"key"`
		Secret string        `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("响应体不是 JSON: %v", err)
	}
	if created.Key.Policy.RPM != 5 || created.Key.Policy.MaxConcurrent != 2 {
		t.Fatalf("策略未落库: %+v", created.Key.Policy)
	}

	rec = call("GET", "/panel/api/keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表 got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hash") || strings.Contains(rec.Body.String(), created.Secret) {
		t.Fatalf("列表泄漏摘要/明文: %s", rec.Body)
	}

	rec = call("POST", "/panel/api/keys/"+created.Key.ID+"/update", `{"disabled":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("更新 disabled 失败: %d %s", rec.Code, rec.Body)
	}

	rec = call("POST", "/panel/api/keys/"+created.Key.ID+"/rotate", `{"grace_hours":1}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "wb2a_") {
		t.Fatalf("轮换应返回新 secret: %d %s", rec.Code, rec.Body)
	}

	rec = call("POST", "/panel/api/keys/"+created.Key.ID+"/delete", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除 got %d body=%s", rec.Code, rec.Body)
	}
	if !store.Empty() {
		t.Fatal("删除后注册表应为空")
	}

	// 未注入注册表 → 501（老部署/单测路径）。
	legacy := New(Config{Version: "test", APIKey: "master"})
	req := httptest.NewRequest("GET", "/panel/api/keys", nil)
	req.Header.Set("Authorization", "Bearer master")
	rec = httptest.NewRecorder()
	legacy.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("无注册表应 501，got %d", rec.Code)
	}
}
