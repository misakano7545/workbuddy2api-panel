package panel

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// issue #287：批量导出账号。导出的文件必须能被本仓自己的导入解析器原样吃回去
// （跨实例迁移是唯一真实用途），所以断言的是 decodeCockpitAccounts 的往返结果。
func TestAccountsExportRoundTripsThroughImport(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"workbuddy-u1.json": `{"accessToken":"at-1","refreshToken":"rt-1","uid":"u1","domain":"www.codebuddy.cn","expiresAt":1800000000}`,
		"workbuddy-g1.json": `{"accessToken":"at-2","refreshToken":"rt-2","uid":"g1","domain":"www.workbuddy.ai","enterpriseId":"e-1"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := New(Config{AuthDir: dir})
	rec := httptest.NewRecorder()
	p.accountsExport(rec, httptest.NewRequest("GET", "/panel/api/accounts/export", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" || cd[:10] != "attachment" {
		t.Fatalf("Content-Disposition = %q（要靠它触发下载）", cd)
	}

	accts, err := decodeCockpitAccounts(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("导出的文件导入侧解不开：%v（body=%s）", err, rec.Body)
	}
	if len(accts) != 2 {
		t.Fatalf("往返后 %d 个账号，want 2：%s", len(accts), rec.Body)
	}
	// 按 uid 排序，凭证与域都得在
	u1, g1 := accts[0], accts[1]
	if u1.UID != "g1" || u1.AccessToken != "at-2" || u1.RefreshToken != "rt-2" ||
		u1.EnterpriseID != "e-1" || u1.Domain != "www.workbuddy.ai" {
		t.Errorf("global 号往返丢了字段：%+v", u1)
	}
	if g1.UID != "u1" || g1.AccessToken != "at-1" || g1.RefreshToken != "rt-1" || g1.ExpiresAt != 1800000000 {
		t.Errorf("cn 号往返丢了字段：%+v", g1)
	}
}
