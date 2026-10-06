package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 暂停选号（paused）的面板接线：POST /pause 与 /resume 必须落到池状态上，
// 且全程不动 disabled（吸收上游 fd835df）。
func TestAccountPauseAndResumeWireToPool(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at1"})
	pn := New(Config{Version: "test", Pool: p})

	call := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		return rec
	}
	if rec := call("/panel/api/accounts/u1/pause"); rec.Code != http.StatusOK {
		t.Fatalf("pause code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Paused {
		t.Errorf("pause 后 Status.Paused 应为 true: %+v", st)
	}
	if st.Disabled {
		t.Errorf("暂停不是禁用：Disabled 必须为 false: %+v", st)
	}
	// 暂停后选号应跳过该号（唯一号 → nil）
	if got := p.Pick(); got != nil {
		t.Errorf("暂停号不应被选中，got %+v", got)
	}

	if rec := call("/panel/api/accounts/u1/resume"); rec.Code != http.StatusOK {
		t.Fatalf("resume code=%d body=%s", rec.Code, rec.Body)
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("resume 后 Paused 应为 false: %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("resume 后应可选，got %+v", got)
	}

	// 未知 uid → 404（供前端区分「账号不存在」）
	if rec := call("/panel/api/accounts/nope/pause"); rec.Code != http.StatusNotFound {
		t.Errorf("pause 未知 uid code=%d want 404", rec.Code)
	}
	if rec := call("/panel/api/accounts/nope/resume"); rec.Code != http.StatusNotFound {
		t.Errorf("resume 未知 uid code=%d want 404", rec.Code)
	}
}

// overview 必须把 paused 透出给面板（前端据此渲染「已暂停选号」与「恢复选号」按钮）。
func TestOverviewExposesPausedFlag(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at1"})
	p.Pause("u1")
	pn := New(Config{Version: "test", Pool: p})

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview code=%d", rec.Code)
	}
	var d struct {
		Disabled int `json:"disabled"`
		Accounts []struct {
			UID    string `json:"uid"`
			Paused bool   `json:"paused"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Accounts) != 1 || !d.Accounts[0].Paused {
		t.Fatalf("overview 未透出 paused: %s", rec.Body)
	}
	if d.Disabled != 1 {
		t.Errorf("暂停号应计入不可用数（disabled=%d want 1）", d.Disabled)
	}
}

// TestAppJSAccountRowPausedBadgeAndButton 面板账号行必须把 paused 渲染成
// 「已暂停选号」标签 + 「恢复选号」按钮，正常号则是「暂停选号」按钮。
//
// 为什么值得一测：s.paused 拼错/字段名漂移时前端会静默退回「可用」+「禁用」，
// 后端全绿而功能在界面上不存在（本仓踩过一次同类的静默渲染回归）。
func TestAppJSAccountRowPausedBadgeAndButton(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; account row paused test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderAccounts('), end = src.indexOf('async function loadOverview(');
assert.ok(start >= 0 && end > start, 'renderAccounts not found');
const tb = { innerHTML: '' };
const ctx = {
  String, Number, Math, Date, Object, Array, JSON, RegExp, console,
  $: id => (id === 'accBody' ? tb : null),
  esc: s => String(s == null ? '' : s), dur: () => '1m', ago: () => '刚刚',
  rateLimitRowsHtml: () => '', formatTokenCount: () => '0', formatLatency: () => '-', formatRate: () => '-',
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderAccounts = renderAccounts;', ctx);
const base = { uid: 'u1', nickname: 'A', credits: 100, credits_total: 100, success_count: 1, err_total: 0 };
ctx.renderAccounts([Object.assign({}, base, { paused: true })]);
assert.ok(tb.innerHTML.includes('已暂停选号'), 'paused 行应显示已暂停选号: ' + tb.innerHTML);
assert.ok(tb.innerHTML.includes('data-a="resume"'), 'paused 行应给「恢复选号」按钮: ' + tb.innerHTML);
assert.ok(!tb.innerHTML.includes('data-a="pause"'), 'paused 行不该再出现「暂停选号」按钮');
ctx.renderAccounts([base]);
assert.ok(tb.innerHTML.includes('data-a="pause"'), '正常行应有「暂停选号」按钮: ' + tb.innerHTML);
assert.ok(tb.innerHTML.includes('data-a="disable"'), '正常行仍应有「禁用」按钮');
assert.ok(!tb.innerHTML.includes('已暂停选号'), '正常行不该出现已暂停选号');
ctx.renderAccounts([Object.assign({}, base, { disabled: true, paused: false })]);
assert.ok(tb.innerHTML.includes('已禁用'), '禁用行仍显示已禁用');
assert.ok(!tb.innerHTML.includes('data-a="disable"'), '禁用行不再显示「禁用」按钮');
console.log('account row paused render passed');`
	path := filepath.Join(t.TempDir(), "accrow.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestAppJSRenderModelLocks 模型锁池表的渲染：空态文案、状态标签、时间与原因转义。
// 数据来自 /panel/api/overview 的 model_locks（后端已排序，前端只展示）。
func TestAppJSRenderModelLocks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model lock render skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const ML_STATE');
assert.ok(start >= 0, 'ML_STATE not found');
const host = { mlBody: { innerHTML: '' }, mlNote: { textContent: '' } };
const ctx = { Object, Array, String, RegExp,
  $: id => host[id],
  esc: s => String(s == null ? '' : s),
  fmtLocalDateTime: ms => 'T(' + ms + ')' };
vm.createContext(ctx);
vm.runInContext(src.slice(start, src.indexOf('async function loadOverview(')) + '\nthis.render = renderModelLocks;', ctx);
// ① 空清单（后端 null）→ 空态文案，不留空表
ctx.render(null);
assert.ok(host.mlBody.innerHTML.includes('当前没有模型级限流'), host.mlBody.innerHTML);
assert.equal(host.mlNote.textContent, '');
// ② 一行 locked：状态标签 + 可选/总数 + 时间渲染 + 原因
ctx.render([{ model: 'glm-5.3', realm: 'cn', state: 'locked', servable: 0, total: 3, locked: 3,
  unlock_at: '2026-10-06T23:00:00+08:00', fully_unlock_at: '2026-10-06T23:17:00+08:00', reason: '6004 model rate limit' }]);
const html = host.mlBody.innerHTML;
assert.ok(html.includes('全池锁定') && html.includes('0 / 3') && html.includes('T(2026-10-06T23:00:00+08:00)'), html);
assert.ok(html.includes('6004 model rate limit'), html);
assert.equal(host.mlNote.textContent, '1 个模型受限');
// ③ 零值时间（上游没给重置时刻）→ 破折号，不显示 0001-01-01
ctx.render([{ model: 'x', realm: 'global', state: 'partial', servable: 1, total: 2, locked: 1,
  unlock_at: '0001-01-01T00:00:00Z', fully_unlock_at: '0001-01-01T00:00:00Z', reason: '' }]);
assert.ok(host.mlBody.innerHTML.includes('部分限流') && host.mlBody.innerHTML.includes('—'), host.mlBody.innerHTML);
assert.ok(!host.mlBody.innerHTML.includes('0001'), '零值时间不得直接显示');
console.log('model lock render passed');`
	path := filepath.Join(t.TempDir(), "mlrender.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// 模型锁池的接线：overview 的 model_locks 必须与池内真实的模型级冷却同源，无锁时为 null。
func TestOverviewExposesModelLocks(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", Nickname: "B", AccessToken: "at2"})
	pn := New(Config{Version: "test", Pool: p})

	read := func() []map[string]any {
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
		if rec.Code != 200 {
			t.Fatalf("overview code=%d", rec.Code)
		}
		var d struct {
			ModelLocks []map[string]any `json:"model_locks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d.ModelLocks
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("无冷却时 model_locks 应为空，得到 %+v", got)
	}
	// 一个号对该模型停车 → 出一个 partial 行（口径与选号一致）。
	p.CooldownSoftForModel("u1", 10*time.Minute, time.Time{}, "glm-5.3", "model rate limit")
	rows := read()
	if len(rows) != 1 || rows[0]["model"] != "glm-5.3" || rows[0]["state"] != "partial" {
		t.Fatalf("model_locks = %+v want 一行 partial glm-5.3", rows)
	}
}

// TestAppJSOverviewWiresModelLocks 接线：loadOverview 必须把 overview 的 model_locks
// 交给 renderModelLocks。少了这一行，锁池表永远空态——渲染函数本身测不到这个缺口。
func TestAppJSOverviewWiresModelLocks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; overview wiring test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function loadOverview(');
const end = src.indexOf("$('accBody').addEventListener");
assert.ok(start >= 0 && end > start, 'loadOverview not found');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; }, apply() { return inert; },
});
let painted = null, accountsPainted = null;
const fake = { version: 'v-test', total: 2, healthy: 2, cooling: 0, disabled: 0, sticky_sessions: 0,
  redis_mode: 'memory', in_flight_full: 0, uptime_sec: 60, accounts: [], model_locks: [{ model: 'glm-5.3', realm: 'cn', state: 'locked' }] };
const ctx = { Math, Object, Array, JSON, Number, String,
  api: async () => fake, $: () => inert, toast: () => {},
  renderAccounts: v => { accountsPainted = v; },
  renderModelLocks: v => { painted = v; } };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.loadOverview = loadOverview;', ctx);
(async () => {
  await ctx.loadOverview(true);
  assert.ok(Array.isArray(accountsPainted), '账号表应被渲染');
  assert.ok(Array.isArray(painted) && painted[0].model === 'glm-5.3', 'overview 的 model_locks 必须交给 renderModelLocks，得到 ' + JSON.stringify(painted));
  process.stdout.write('overview wiring passed\n');
})().catch(e => { console.error(e); process.exit(1); });`
	path := filepath.Join(t.TempDir(), "mlwire.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
