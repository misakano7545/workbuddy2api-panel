package upstream

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestZZLiveProbe257 真机复核 issue #257 的实现结果：injectClientIdentityHeaders 上线后，
// 用真实账号打一次 RPC 族与 chat，确认
//
//  1. 请求照旧被上游接受（不是「加了头就被拦」）；
//  2. 实际发出的头集与 realm 一致——国内版带 X-Client-*，国际版一个都不带。
//
// 只在设了 WB_PROBE_AUTHDIR 时有意义；普通 go test 自动跳过。
//
//	GOOS=linux GOARCH=amd64 go test -c -o x.test ./internal/upstream
//	WB_PROBE_AUTHDIR=/opt/.../auths WB_PROBE_UID=<uid> ./x.test -test.run TestZZLiveProbe257 -test.v
//
// WB_PROBE_MODEL 可指定 chat 用的模型（默认 deepseek-v4.1-flash）。
// 抓包证据与处置口径见 docs/issue-257-client-identity.md。
func TestZZLiveProbe257(t *testing.T) {
	dir := os.Getenv("WB_PROBE_AUTHDIR")
	if dir == "" {
		t.Skip("未设 WB_PROBE_AUTHDIR，跳过真机探针")
	}
	acct := loadProbeAuth(t, dir, os.Getenv("WB_PROBE_UID"))
	realm := acct.Realm()
	t.Logf("账号 %s realm=%s domain=%s", acct.UID, realm, acct.DomainValue())

	rec := &probeRecorder{}
	c := New()
	rec.base = c.HTTP.Transport
	c.HTTP = &http.Client{Timeout: 90 * time.Second, Transport: rec}
	c.ChatHTTP = &http.Client{Timeout: 90 * time.Second, Transport: rec}

	if err := c.RefreshTokenIfNeeded(acct, 10*time.Minute); err != nil {
		t.Fatalf("刷新 token 失败: %v", err)
	}

	var lines []string
	add := func(name string, err error, path string) {
		lines = append(lines, fmt.Sprintf("  %-28s err=%v  %s", name, err, rec.line(path)))
	}

	add("/v2/report", c.ReportDesktopEvent(acct, DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
	}), "/v2/report")
	remain, total, err := c.UserResource(acct)
	add("billing 余额", err, "/billing/meter/get-user-resource-summary")
	t.Logf("余额 %d/%d", remain, total)
	models, err := c.FetchModels(acct)
	add("/v3/config", err, "/v3/config")
	t.Logf("模型数 %d", len(models))
	status, _, err := probeChat(t, c, acct, os.Getenv("WB_PROBE_MODEL"))
	add("/v2/chat/completions", err, "/v2/chat/completions")
	if status != 200 {
		t.Errorf("chat status=%d（加头之后被上游拒绝？）", status)
	}

	fmt.Printf("\n===== realm=%s =====\n%s\n", realm, strings.Join(lines, "\n"))

	// 头集与 realm 的一致性是本探针要证明的不变量。
	for path, h := range rec.snapshot() {
		got := h.Get("X-Client-Version") != "" && h.Get("X-Client-Platform") != "" && h.Get("X-Client-Product") != ""
		if realm == "global" && got {
			t.Errorf("国际版 %s 不该发 X-Client-*", path)
		}
		if realm == "cn" && !got {
			t.Errorf("国内版 %s 缺 X-Client-*", path)
		}
	}
}

// probeRecorder 在 Transport 层记录每个路径最后一次请求的头与状态码——断言走的是
// 完全相同的生产代码路径，不是另搓一份请求。
type probeRecorder struct {
	base http.RoundTripper

	mu      sync.Mutex
	status  map[string]int
	headers map[string]http.Header
}

func (p *probeRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := p.base.RoundTrip(req)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == nil {
		p.status = map[string]int{}
		p.headers = map[string]http.Header{}
	}
	p.headers[req.URL.Path] = req.Header.Clone()
	if err == nil && resp != nil {
		p.status[req.URL.Path] = resp.StatusCode
	} else {
		p.status[req.URL.Path] = -1
	}
	return resp, err
}

func (p *probeRecorder) snapshot() map[string]http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]http.Header, len(p.headers))
	for k, v := range p.headers {
		out[k] = v
	}
	return out
}

func (p *probeRecorder) line(path string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.status[path]
	if !ok {
		return "(未发出该请求)"
	}
	h := p.headers[path]
	var have []string
	for _, k := range []string{"X-Client-Version", "X-Client-Platform", "X-Client-Product", "X-Device-Token"} {
		if h.Get(k) != "" {
			have = append(have, k)
		}
	}
	mark := "无 X-Client-*"
	if len(have) > 0 {
		mark = "带 " + strings.Join(have, ",")
	}
	return fmt.Sprintf("status=%d ua=%q %s", st, h.Get("User-Agent"), mark)
}

// probeChat 用最小 body 打一次 chat，只为看是否 200。
func probeChat(t *testing.T, c *Client, a *auth.Auth, model string) (int, []byte, error) {
	t.Helper()
	if model == "" {
		model = "deepseek-v4.1-flash"
	}
	body := fmt.Sprintf(`{"model":%q,"stream":true,`+
		`"messages":[{"role":"user","content":"只回复 OK"}]}`, model)
	rc, status, respBody, err := c.ChatStream(a, []byte(body), "127.0.0.1", ChatMeta{})
	if rc != nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}
	if err != nil && len(respBody) > 0 {
		t.Logf("chat 错误体: %.240s", string(respBody))
	}
	return status, respBody, err
}

// loadProbeAuth 从 auth 目录里挑一个账号（uid 含 WB_PROBE_UID 的那个）。
func loadProbeAuth(t *testing.T, dir, uid string) *auth.Auth {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 auth 目录: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if uid != "" && !strings.Contains(e.Name(), uid) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			t.Logf("跳过 %s: %v", e.Name(), err)
			continue
		}
		// FilePath 必须补：auth 的 SaveAtomic 与 backfill 都靠它定位原文件。
		a.FilePath = filepath.Join(dir, e.Name())
		return a
	}
	t.Fatalf("auth 目录 %s 里找不到账号 uid=%q", dir, uid)
	return nil
}
