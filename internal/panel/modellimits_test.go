package panel

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/ratelimit"
)

// mlTracker 造一个带真实策略文件路径的 tracker（落盘路径不能为空，否则 Save 会报
// 「模型限流映射策略路径为空」，改策略的用例就全废）。
func mlTracker(t *testing.T) *ratelimit.Tracker {
	t.Helper()
	return ratelimit.NewTracker(ratelimit.TrackerConfig{
		Path: filepath.Join(t.TempDir(), "model_limits.json"),
		Deps: ratelimit.Deps{
			Usage:    func(time.Duration) []ratelimit.UsageRow { return nil },
			Accounts: func() []ratelimit.Account { return nil },
			Realms:   func() []string { return []string{"cn", "global"} },
			Rates:    func(string) map[string]string { return map[string]string{"m1": "1.0"} },
			Models:   func(string) []string { return []string{"m1"} },
			Fallback: func() map[string]string { return nil },
			Now:      time.Now,
		},
	})
}

// mlPanel 造一个带限流 tracker 的面板（鉴权走 test-key）。
func mlPanel(t *testing.T, tr *ratelimit.Tracker) *Panel {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "小明"})
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, ModelLimits: tr})
}

// mlDo 打一个面板请求并返回状态码与响应体。
func mlDo(t *testing.T, p *Panel, method, path, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, r)
	return rr.Code, rr.Body.String()
}

// 未启用（tracker 为 nil）：统一 503 + 明确文案。
// 这里不能用空表糊弄：空表会被读成「额度全用光了」，与事实完全相反。
func TestModelLimitsDisabledReturns503(t *testing.T) {
	code, body := mlDo(t, mlPanel(t, nil), "GET", "/panel/api/model_limits", "")
	if code != 503 {
		t.Fatalf("tracker 为 nil 应返回 503，得到 %d: %s", code, body)
	}
	if !strings.Contains(body, "模型限流映射未启用") {
		t.Fatalf("应说明未启用，得到 %s", body)
	}
}

// GET：返回视图 + 降级表；降级表为空时是 null 而不是 {}。
func TestModelLimitsGETReturnsView(t *testing.T) {
	code, body := mlDo(t, mlPanel(t, mlTracker(t)), "GET", "/panel/api/model_limits", "")
	if code != 200 {
		t.Fatalf("GET model_limits 应为 200，得到 %d: %s", code, body)
	}
	var out struct {
		OK       bool              `json:"ok"`
		Error    string            `json:"error"`
		View     ratelimit.View    `json:"view"`
		Fallback map[string]string `json:"fallback"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if !out.OK {
		t.Fatalf("ok 应为 true: %s", body)
	}
	if out.Error != "" {
		t.Fatalf("新装的 tracker 不应有加载错误，得到 %q", out.Error)
	}
	if len(out.View.Presets) == 0 {
		t.Fatalf("视图应带内置预设: %s", body)
	}
	if out.Fallback != nil {
		t.Fatalf("未配置降级表时应为 null，得到 %v", out.Fallback)
	}
}

// ?refresh=1 强制重算快照：不重算时 View.Generated 保持旧值，重算后必须刷新。
// 页面「刷新」按钮带这个参数；没有它时运营刚改的池子要等后台 30s 才能看到。
func TestModelLimitsGETRefreshForcesRebuild(t *testing.T) {
	tr := mlTracker(t)
	p := mlPanel(t, tr)
	// 先把初始快照记下来，再让时间往前走，确保重算后 Generated 真的不同。
	_, body := mlDo(t, p, "GET", "/panel/api/model_limits", "")
	var before struct {
		View ratelimit.View `json:"view"`
	}
	if err := json.Unmarshal([]byte(body), &before); err != nil {
		t.Fatalf("首次 GET 响应不是合法 JSON: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // RFC3339 秒精度，睡够 1s 才看得到差异
	code, body := mlDo(t, p, "GET", "/panel/api/model_limits?refresh=1", "")
	if code != 200 {
		t.Fatalf("?refresh=1 应为 200，得到 %d: %s", code, body)
	}
	var after struct {
		View ratelimit.View `json:"view"`
	}
	if err := json.Unmarshal([]byte(body), &after); err != nil {
		t.Fatalf("刷新响应不是合法 JSON: %v", err)
	}
	if !after.View.Generated.After(before.View.Generated) {
		t.Fatalf("?refresh=1 没有重算快照：before=%s after=%s",
			before.View.Generated.Format(time.RFC3339), after.View.Generated.Format(time.RFC3339))
	}
}

// 方法不对时 405（不要静默按 GET 处理）。
func TestModelLimitsWrongMethod(t *testing.T) {
	code, body := mlDo(t, mlPanel(t, mlTracker(t)), "POST", "/panel/api/model_limits", "{}")
	if code != 405 {
		t.Fatalf("GET 端点收到 POST 应为 405，得到 %d: %s", code, body)
	}
}

// upsert 后 View 里的策略要真的变了 —— 面板改完策略，网关按的就是这份策略，
// 页面不反映改动等于改了个寂寞。
func TestModelLimitsPolicyUpsertChangesView(t *testing.T) {
	tr := mlTracker(t)
	p := mlPanel(t, tr)

	body := `{"op":"upsert","limit":{"realm":"cn","model":"m1","source":"manual","window_sec":3600,"token_quota_per_account":1000,"on_hit":"hold"}}`
	code, resp := mlDo(t, p, "POST", "/panel/api/model_limits/policy", body)
	if code != 200 {
		t.Fatalf("upsert 应为 200，得到 %d: %s", code, resp)
	}
	var out struct {
		OK   bool           `json:"ok"`
		View ratelimit.View `json:"view"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, resp)
	}
	if !out.OK {
		t.Fatalf("ok 应为 true: %s", resp)
	}
	if len(out.View.Policy.Overrides) != 1 {
		t.Fatalf("策略里应有 1 条覆盖项: %s", resp)
	}
	o := out.View.Policy.Overrides[0]
	if o.Realm != "cn" || o.Model != "m1" || o.TokenQuotaPerAccount != 1000 || o.OnHit != "hold" {
		t.Fatalf("覆盖项内容不符: %+v", o)
	}
	// tracker 自身的策略也要跟着变（面板与网关共用同一实例）。
	if got := tr.Policy(); len(got.Overrides) != 1 || got.Overrides[0].Model != "m1" {
		t.Fatalf("tracker 策略未生效: %+v", got)
	}
}

// 非法策略要 400（写坏的策略比没有这个功能更糟）。
func TestModelLimitsPolicyRejectsBadLimit(t *testing.T) {
	code, body := mlDo(t, mlPanel(t, mlTracker(t)), "POST", "/panel/api/model_limits/policy", `{"op":"upsert","limit":{"realm":"mars","model":"m1"}}`)
	if code != 400 {
		t.Fatalf("未知域应为 400，得到 %d: %s", code, body)
	}
}

// 未知 op 必须报错而不是默默忽略（默默忽略会让页面以为改成功了）。
func TestModelLimitsPolicyRejectsUnknownOp(t *testing.T) {
	code, body := mlDo(t, mlPanel(t, mlTracker(t)), "POST", "/panel/api/model_limits/policy", `{"op":"nope"}`)
	if code != 400 {
		t.Fatalf("未知 op 应为 400，得到 %d: %s", code, body)
	}
}

// preview：不带草稿时预览当前策略；带草稿时预览「保存后会怎样」。
func TestModelLimitsPreview(t *testing.T) {
	tr := mlTracker(t)
	p := mlPanel(t, tr)

	code, resp := mlDo(t, p, "POST", "/panel/api/model_limits/preview", `{"realm":"cn","model":"m1"}`)
	if code != 200 {
		t.Fatalf("preview 应为 200，得到 %d: %s", code, resp)
	}
	var out struct {
		OK      bool              `json:"ok"`
		Preview ratelimit.Preview `json:"preview"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, resp)
	}
	if !out.OK || out.Preview.Realm != "cn" || out.Preview.Model != "m1" {
		t.Fatalf("预览内容不符: %s", resp)
	}

	// 带草稿：草稿叠进策略副本后预览，额度应按草稿算。
	code, resp = mlDo(t, p, "POST", "/panel/api/model_limits/preview", `{"realm":"cn","model":"m1","limit":{"realm":"cn","model":"m1","source":"manual","window_sec":3600,"token_quota_per_account":12345,"on_hit":"hold"}}`)
	if code != 200 {
		t.Fatalf("带草稿的 preview 应为 200，得到 %d: %s", code, resp)
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, resp)
	}
	if out.Preview.Quota.TokenPerAccount != 12345 {
		t.Fatalf("草稿额度应生效（12345），得到 %d: %s", out.Preview.Quota.TokenPerAccount, resp)
	}
	// 预览不能改策略：tracker 里的策略必须还是原样。
	if got := tr.Policy(); len(got.Overrides) != 0 {
		t.Fatalf("预览不应写入策略: %+v", got)
	}
}

// export：整包 JSON 与 markdown 两种形态。
func TestModelLimitsExport(t *testing.T) {
	p := mlPanel(t, mlTracker(t))

	code, resp := mlDo(t, p, "POST", "/panel/api/model_limits/export", "{}")
	if code != 200 {
		t.Fatalf("export 应为 200，得到 %d: %s", code, resp)
	}
	var out struct {
		OK     bool             `json:"ok"`
		Bundle ratelimit.Bundle `json:"bundle"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, resp)
	}
	if !out.OK || out.Bundle.Version == 0 {
		t.Fatalf("导出包应有版本号: %s", resp)
	}

	code, resp = mlDo(t, p, "POST", "/panel/api/model_limits/export", `{"format":"markdown"}`)
	if code != 200 {
		t.Fatalf("markdown 导出应为 200，得到 %d: %s", code, resp)
	}
	var md struct {
		OK       bool   `json:"ok"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(resp), &md); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, resp)
	}
	if !md.OK || md.Markdown == "" {
		t.Fatalf("markdown 内容为空: %s", resp)
	}
}
