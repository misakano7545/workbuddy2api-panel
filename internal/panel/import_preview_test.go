package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// importPreviewEnv 面板导入的测试环境：上游一个端点都不实现（全 404），所以任何
// 一次真正的上游请求都会被 refreshCalls 之类的计数器抓到。预览路径本不该发请求。
type importPreviewEnv struct {
	panel        *Panel
	dir          string
	refreshCalls int
	upstreamHits int
}

func newImportPreviewEnv(t *testing.T) *importPreviewEnv {
	t.Helper()
	e := &importPreviewEnv{dir: t.TempDir()}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc2(func(r *http.Request) (*http.Response, error) {
			e.upstreamHits++
			if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
				e.refreshCalls++
			}
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":404,"msg":"not found"}`)),
			}, nil
		})},
		ChatBaseCN: "https://cn.example", WebBaseCN: "https://cn.example", BillingBaseCN: "https://cn.example",
	}
	e.panel = New(Config{Pool: pool.New(""), Upstream: up, AuthDir: e.dir})
	return e
}

// post 按表单顺序发多个文件；dryRun 非空时一并带上。
func (e *importPreviewEnv) post(t *testing.T, dryRun string, files map[string][]byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// map 本身无序，这里用固定顺序的键列表保证可断言的决定性行为。
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		raw, ok := files[name]
		if !ok {
			continue
		}
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if dryRun != "" {
		if err := mw.WriteField("dryRun", dryRun); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

type importPreviewResp struct {
	DryRun     bool     `json:"dry_run"`
	Total      int      `json:"total"`
	Imported   int      `json:"imported"`
	Skipped    int      `json:"skipped"`
	Creates    int      `json:"creates"`
	Overwrites int      `json:"overwrites"`
	Errors     []string `json:"errors"`
}

func decodePreview(t *testing.T, body string) importPreviewResp {
	t.Helper()
	var r importPreviewResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode response: %v (%s)", err, body)
	}
	return r
}

func cockpitFile(t *testing.T, rows ...map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 预览必须零副作用：不落盘、不发上游请求（尤其是不能真去 refresh——那会轮换掉
// 单次有效的 RT）、不改账号池。变异判据：把 dryRun 早退那段删掉，本用例会红在
// refreshCalls / auths 目录 / 池大小三条上。
func TestImportPreviewHasNoSideEffects(t *testing.T) {
	e := newImportPreviewEnv(t)
	// 池里已有 u9，磁盘上也有它的凭据：预览必须原样保留。
	existingPath := filepath.Join(e.dir, "workbuddy-u9.json")
	old := &auth.Auth{UID: "u9", AccessToken: "old-at", RefreshToken: "old-rt",
		ExpiresAt: time.Now().Add(48 * time.Hour).Unix(), FilePath: existingPath}
	if err := old.SaveAtomic(); err != nil {
		t.Fatalf("prepare existing credential: %v", err)
	}
	e.panel.cfg.Pool.Add(old)

	body := cockpitFile(t,
		map[string]any{"uid": "u1", "access_token": "at1", "refresh_token": "rt1", "expires_at": time.Now().UnixMilli()},
		map[string]any{"uid": "u2", "access_token": "at2", "refresh_token": "rt2", "expires_at": time.Now().UnixMilli()},
		map[string]any{"uid": "u9", "access_token": "new-at", "refresh_token": "new-rt", "expires_at": time.Now().UnixMilli()},
	)
	code, out := e.post(t, "true", map[string][]byte{"a.json": body})
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, out)
	}
	got := decodePreview(t, out)
	if !got.DryRun {
		t.Fatalf("dry_run 应为 true：%s", out)
	}
	if got.Total != 3 || got.Imported != 3 || got.Skipped != 0 {
		t.Fatalf("total/imported/skipped = %d/%d/%d want 3/3/0（%s）", got.Total, got.Imported, got.Skipped, out)
	}
	if got.Creates != 2 || got.Overwrites != 1 {
		t.Fatalf("creates/overwrites = %d/%d want 2/1（%s）", got.Creates, got.Overwrites, out)
	}
	if e.refreshCalls != 0 || e.upstreamHits != 0 {
		t.Fatalf("预览发了上游请求：refresh=%d total=%d", e.refreshCalls, e.upstreamHits)
	}
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if ent.Name() != "workbuddy-u9.json" {
			t.Fatalf("预览写入了文件：%s", ent.Name())
		}
	}
	if n := len(e.panel.cfg.Pool.List()); n != 1 {
		t.Fatalf("预览改变了账号池：pool=%d want 1", n)
	}
	saved, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "old-at") {
		t.Fatalf("预览覆盖了已有凭据：%s", saved)
	}
}

// 多文件：按表单顺序解码，批内重复 uid 只保留第一次出现的那条，并说明来源文件。
func TestImportPreviewMultiFileAndDedupe(t *testing.T) {
	e := newImportPreviewEnv(t)
	a := cockpitFile(t,
		map[string]any{"uid": "u1", "access_token": "at1", "refresh_token": "rt1", "expires_at": time.Now().UnixMilli()},
	)
	b := cockpitFile(t,
		map[string]any{"uid": "u1", "access_token": "at1b", "refresh_token": "rt1b", "expires_at": time.Now().UnixMilli()},
		map[string]any{"uid": "u2", "access_token": "at2", "refresh_token": "rt2", "expires_at": time.Now().UnixMilli()},
		map[string]any{"uid": "u3", "access_token": "", "refresh_token": "rt3"},
	)
	code, out := e.post(t, "1", map[string][]byte{"a.json": a, "b.json": b})
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, out)
	}
	got := decodePreview(t, out)
	if got.Total != 4 || got.Imported != 2 || got.Skipped != 2 {
		t.Fatalf("total/imported/skipped = %d/%d/%d want 4/2/2（%s）", got.Total, got.Imported, got.Skipped, out)
	}
	joined := strings.Join(got.Errors, " | ")
	if !strings.Contains(joined, "b.json") || !strings.Contains(joined, "duplicate uid=u1") {
		t.Fatalf("重复 uid 应指名来源文件：%s", joined)
	}
	if !strings.Contains(joined, "b.json: missing required fields") {
		t.Fatalf("缺字段应指名来源文件：%s", joined)
	}
	if e.upstreamHits != 0 {
		t.Fatalf("预览发了上游请求：%d", e.upstreamHits)
	}
}

// 任一文件不可用时整体中止：不做「先导入前面那部分」。
func TestImportMultiFileAbortsWhenOneFileBroken(t *testing.T) {
	e := newImportPreviewEnv(t)
	good := cockpitFile(t, map[string]any{
		"uid": "u1", "access_token": "at1", "refresh_token": "rt1", "expires_at": time.Now().UnixMilli(),
	})
	code, out := e.post(t, "true", map[string][]byte{"a.json": good, "b.json": []byte(`{"broken":`)})
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（%s）", code, out)
	}
	if !strings.Contains(out, "b.json") {
		t.Fatalf("错误应指名坏掉的文件：%s", out)
	}

	code, out = e.post(t, "true", map[string][]byte{"a.json": good, "b.json": []byte(`{"accounts":[]}`)})
	if code != http.StatusBadRequest || !strings.Contains(out, "empty accounts array") {
		t.Fatalf("空账号列表应整体 400：code=%d %s", code, out)
	}
	if e.upstreamHits != 0 {
		t.Fatalf("中止路径不应发上游请求：%d", e.upstreamHits)
	}
}

// 真实导入（不打 dryRun）照旧落盘：多文件都能进池，已有的那份保留校验行为。
func TestImportMultiFileCommitsWhenNotDryRun(t *testing.T) {
	e := newImportPreviewEnv(t)
	a := cockpitFile(t, map[string]any{
		"uid": "u1", "access_token": "at1", "refresh_token": "rt1", "expires_at": time.Now().UnixMilli(),
	})
	b := cockpitFile(t, map[string]any{
		"uid": "u2", "access_token": "at2", "refresh_token": "rt2", "expires_at": time.Now().UnixMilli(),
	})
	code, out := e.post(t, "", map[string][]byte{"a.json": a, "b.json": b})
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, out)
	}
	got := decodePreview(t, out)
	if got.DryRun {
		t.Fatalf("未带 dryRun 不应标成预览：%s", out)
	}
	if got.Imported != 2 || got.Skipped != 0 || got.Creates != 2 {
		t.Fatalf("imported/skipped/creates = %d/%d/%d want 2/0/2（%s）",
			got.Imported, got.Skipped, got.Creates, out)
	}
	for _, uid := range []string{"u1", "u2"} {
		if _, err := os.Stat(filepath.Join(e.dir, "workbuddy-"+uid+".json")); err != nil {
			t.Fatalf("%s 未落盘：%v", uid, err)
		}
	}
	if n := len(e.panel.cfg.Pool.List()); n != 2 {
		t.Fatalf("pool=%d want 2", n)
	}
}
