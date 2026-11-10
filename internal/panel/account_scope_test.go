package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// TestAccountScopeEndpoint 号池标注接口（POST /panel/api/accounts/{uid}/scope）。
//
// 与 group 端点同构，但**取值必须校验**：写进一个不认识的字符串会被
// pool.NormalizeScope 悄悄归一成 text，运维以为标了媒体号、实际还是文本号——
// 那是最难查的一类错配置，所以这里宁可直接 400。
func TestAccountScopeEndpoint(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})

	call := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("/panel/api/accounts/u1/scope", `{"scope":"media"}`); rec.Code != http.StatusOK {
		t.Fatalf("标媒体号应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Scope != pool.ScopeMedia {
		t.Fatalf("池里没落号池标注: %q", st.Scope)
	}
	if got := pl.ScopeOf("u1"); got != pool.ScopeMedia {
		t.Fatalf("ScopeOf=%q want media", got)
	}

	// 空串 = 清回缺省（文本号池），响应里回显归一后的值。
	rec := call("/panel/api/accounts/u1/scope", `{"scope":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空应 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"scope":"text"`) {
		t.Fatalf("清空后应回显 text: %s", rec.Body.String())
	}
	if got := pl.ScopeOf("u1"); got != pool.ScopeText {
		t.Fatalf("清空后应回落 text, got %q", got)
	}

	// 未知取值 400（不能静默归一）。
	if rec := call("/panel/api/accounts/u1/scope", `{"scope":"video"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知号池应 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	// 账号不存在 404。
	if rec := call("/panel/api/accounts/nope/scope", `{"scope":"media"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("未知账号应 404, got %d", rec.Code)
	}
	// 坏 body 400。
	if rec := call("/panel/api/accounts/u1/scope", `not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 body 应 400, got %d", rec.Code)
	}
}
