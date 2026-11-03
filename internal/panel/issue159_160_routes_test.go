package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/reqlog"
)

// TestAccountGroupEndpoint 分组接口真的挂上了路由（issue #159）。
//
// 池层的 SetGroup 有单测，但「路由没注册 / 路径写错 / body 没解」这三种错法在 Go
// 编译与池层测试里都看不见——面板上表现为点了保存什么都没发生。
func TestAccountGroupEndpoint(t *testing.T) {
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

	if rec := call("/panel/api/accounts/u1/group", `{"group":"批量A"}`); rec.Code != http.StatusOK {
		t.Fatalf("设置分组应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Group != "批量A" {
		t.Fatalf("池里没落分组: %q", st.Group)
	}
	// 不存在账号回 404（否则 uid 打错时界面上看起来「保存成功」）。
	if rec := call("/panel/api/accounts/nope/group", `{"group":"A"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("未知账号应 404, got %d", rec.Code)
	}
	if rec := call("/panel/api/accounts/u1/group", `not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 body 应 400, got %d", rec.Code)
	}
}

// TestRequestLogsClearEndpoint 清空归档接口：删文件 + 之后读回为空（issue #160）。
func TestRequestLogsClearEndpoint(t *testing.T) {
	rl := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), RetentionDays: 7})
	rl.Record(reqlog.Event{Time: time.Now().Add(-time.Minute), RequestID: "a", Status: 200, OK: true})
	p := New(Config{Version: "test", APIKey: "k", RequestLog: rl})

	req := httptest.NewRequest("POST", "/panel/api/request_logs/clear", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"files":1`) {
		t.Fatalf("应报删除文件数, got %s", rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/panel/api/request_logs?limit=10", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取应 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Fatalf("清空后应读到空列表, got %s", rec.Body.String())
	}
}
