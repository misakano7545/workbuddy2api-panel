package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 面板静态资源（测试工作目录 = internal/panel）。
func readModellimitsAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", name, err)
	}
	return string(b)
}

// TestModelLimitsFrontendMarkup 页面骨架：导航项、视图 section、三对标记块，
// 以及工具条 / 表格 / 弹窗上 js 会用到的每个 id 都在 HTML 里存在。
func TestModelLimitsFrontendMarkup(t *testing.T) {
	html := readModellimitsAsset(t, "index.html")
	for _, want := range []string{
		`href="#modellimits" data-view="modellimits"`,
		`<section class="view" id="view-modellimits" hidden>`,
		`id="mrlPreset"`,
		`id="mrlRealm"`,
		`id="mrlQ"`,
		`id="mrlTight"`,
		`id="mrlManual"`,
		`id="btnMrlNew"`,
		`id="btnMrlExportJson"`,
		`id="btnMrlExportMd"`,
		`id="btnMrlRefresh"`,
		`id="btnMrlReset"`,
		`id="mrlSummary"`,
		`id="mrlBody"`,
		`id="mrlVeil"`,
		`id="mrlDlgSource"`,
		`id="mrlDlgOnHit"`,
		`id="mrlDlgChainMode"`,
		`id="mrlDlgChain"`,
		`id="mrlPreview"`,
		`id="btnMrlPreview"`,
		`id="btnMrlSave"`,
		`id="btnMrlDelete"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %s", want)
		}
	}
	// 导航顺序：「模型限流映射」排在「模型规则」之后（图标风格照抄邻居）。
	rules := strings.Index(html, `data-view="modelrules"`)
	limits := strings.Index(html, `data-view="modellimits"`)
	if rules < 0 || limits < rules {
		t.Errorf("导航顺序不对：modelrules 在第 %d 字节、modellimits 在第 %d 字节", rules, limits)
	}
	// 三个标记块必须成对（开始在前、结束在后，且各只出现一次）。
	for _, pair := range [][2]string{
		{"/* >>> modellimits-css", "/* <<< modellimits-css"},
		{"<!-- >>> modellimits-view", "<!-- <<< modellimits-view"},
		{"<!-- >>> modellimits-dlg", "<!-- <<< modellimits-dlg"},
	} {
		start, end := strings.Index(html, pair[0]), strings.Index(html, pair[1])
		if n := strings.Count(html, pair[0]); n != 1 {
			t.Errorf("标记 %q 出现 %d 次，应为 1 次", pair[0], n)
		}
		if n := strings.Count(html, pair[1]); n != 1 {
			t.Errorf("标记 %q 出现 %d 次，应为 1 次", pair[1], n)
		}
		if start < 0 || end < 0 || end < start {
			t.Errorf("标记块 %q … %q 不成对（start=%d end=%d）", pair[0], pair[1], start, end)
		}
	}
}

// TestModelLimitsAppWiring app.js 侧接线：TITLES 键、go() 进入即加载、
// refreshVisible 的轮询分支、四个端点，以及 CSP 相关的禁用项。
func TestModelLimitsAppWiring(t *testing.T) {
	js := readModellimitsAsset(t, "app.js")
	for _, want := range []string{
		"modellimits: '模型限流映射'",
		"if (v === 'modellimits') loadModelLimits();",
		"else if (view === 'modellimits') loadModelLimits(true);",
		"function limitRowHtml(",
		"function limitDetailHtml(",
		"function chainHtml(",
		"function policyPayload(",
		"async function loadModelLimits(",
		"function applyMrlView(",
		"// >>> modellimits-app",
		"// <<< modellimits-app",
		"// >>> modellimits-pure",
		"// <<< modellimits-pure",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %s", want)
		}
	}

	start := strings.Index(js, "// >>> modellimits-app")
	end := strings.Index(js, "// <<< modellimits-app")
	if start < 0 || end < start {
		t.Fatalf("modellimits-app 标记块不成对（start=%d end=%d）", start, end)
	}
	block := js[start:end]

	// 前端只许调 internal/ratelimit/README.md 里的四个端点。
	allowed := map[string]bool{
		"model_limits":         true,
		"model_limits/policy":  true,
		"model_limits/preview": true,
		"model_limits/export":  true,
	}
	rest := block
	for {
		i := strings.Index(rest, "api('")
		if i < 0 {
			break
		}
		rest = rest[i+len("api('"):]
		j := strings.Index(rest, "'")
		if j < 0 {
			t.Fatalf("modellimits 块里有未闭合的 api(' 调用")
		}
		path := rest[:j]
		if !allowed[path] {
			t.Errorf("前端调了契约外的端点 %q（只允许 model_limits 开关这四个）", path)
		}
	}

	// CSP 严格：不写 eval、不写 inline 事件属性，事件统一 addEventListener。
	for _, banned := range []string{"eval(", "onclick=\"", "onchange=\"", "javascript:"} {
		if strings.Contains(block, banned) {
			t.Errorf("modellimits 块里出现 %q（CSP 严格，事件必须 addEventListener）", banned)
		}
	}
	// 导出下载必须走 Blob + a.download（不新开窗口）。
	if !strings.Contains(block, "new Blob(") || !strings.Contains(block, "a.download") {
		t.Errorf("导出没有走 Blob + a.download 下载")
	}
}

// TestModelLimitsRender 用 node + vm 单独跑纯渲染函数：行、降级链、
// 账号明细折叠、表单载荷（policy/upsert 与 preview 共用的那份口径）。
func TestModelLimitsRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model limits render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('// >>> modellimits-pure');
const end = src.indexOf('// <<< modellimits-pure');
if (start < 0 || end < start) throw new Error('modellimits pure region not found');
const ctx = {
  esc: s => String(s == null ? '' : s),
  formatTokenCount: n => (n == null ? '—' : String(n)),
};
vm.createContext(ctx);
vm.runInContext("'use strict';" +
  src.slice(start, end) +
  'this.limitRowHtml=limitRowHtml;this.limitDetailHtml=limitDetailHtml;' +
  'this.chainHtml=chainHtml;this.policyPayload=policyPayload;', ctx);
const NL = String.fromCharCode(10);
const base = {
  realm: 'cn', model: 'deepseek-v4.1-flash', full_name: 'cn:deepseek-v4.1-flash',
  rate: '1', rate_value: 1, has_rate: true, zero_rate: false,
  source: 'manual', on_hit: 'degrade', chain_mode: 'zero', window_sec: 86400,
  quota: { window_sec: 86400, token_per_account: 200000000, token_per_pool: 400000000, inferred: true },
  usage: { tokens: 1000, requests: 4, errors: 0, credits: 1.5 },
  pool: { quota_tokens: 400000000, used_tokens: 360000000, remaining_tokens: 40000000, pct: 0.9 },
  accounts: [{ uid: '1001', nickname: '小号', routable: true, tokens: 1000, quota_tokens: 200000000,
    remaining_tokens: 199999000, pct: 0.000005, limited: false, credits_left: 60 }],
  limit_state: 'partial', limited_accounts: 1, routable_accounts: 2,
  chain: [{ model: 'hy4flash-free', rate: '0', rate_value: 0, has_rate: true, zero_rate: true, reason: '自动：零倍率免费档' },
          { model: 'hy4flash', rate: '0.5', rate_value: 0.5, has_rate: true, zero_rate: false, reason: '兜底' }],
  chain_source: 'auto-zero', degrading: true,
};
const cp = o => Object.assign({}, base, o);
const html = ctx.limitRowHtml(base);
const freeRow = ctx.limitRowHtml(cp({ zero_rate: true, rate: '0', source: 'follow',
  pool: { quota_tokens: 400000000, used_tokens: 388000000, remaining_tokens: 12000000, pct: 0.97 } }));
const noChain = ctx.limitRowHtml(cp({ chain: [], chain_source: 'none' }));
const accs = [];
for (let i = 0; i < 10; i++) accs.push({ uid: '20' + i, routable: true, tokens: 10, remaining_tokens: 5, pct: 0.5, limited: false, credits_left: 1 });
const detail = ctx.limitDetailHtml(base, false);
const many = ctx.limitDetailHtml(cp({ accounts: accs }), false);
const manyAll = ctx.limitDetailHtml(cp({ accounts: accs }), true);
const body = ctx.policyPayload({ realm: 'cn', model: 'x', source: 'manual', window_sec: '7200',
  token_quota_per_account: '1000000', credit_quota_per_account: '2.5', credit_quota_per_pool: '10',
  on_hit: 'hold', chain_mode: 'zero', chain: 'a, b' + NL + 'c，d', note: ' hi ' });
const bare = ctx.policyPayload({ realm: 'cn' });
process.stdout.write(JSON.stringify({
  render: {
    manual: html.includes('手动'),
    freePill: freeRow.includes('免费'),
    warnBar: html.includes('ml-bar warn'),
    badBar: freeRow.includes('ml-bar bad'),
    partialTag: html.includes('部分吃紧'),
    degradingTag: html.includes('降级中'),
    arrow: html.includes('→'),
    freeStep: html.includes('st free'),
    chainSource: html.includes('自动零倍率'),
    noChain: noChain.includes('不降级'),
    editBtn: html.includes('data-act="edit"'),
    rowKey: html.includes('data-key="cn|deepseek-v4.1-flash"'),
    detailTable: detail.includes('ml-acct'),
    detailUid: detail.includes('1001'),
    detailKey: detail.includes('data-det="cn|deepseek-v4.1-flash"'),
    foldCut: (many.match(/正常/g) || []).length,
    foldNote: many.includes('展开全部 10 个'),
    unfoldRows: (manyAll.match(/正常/g) || []).length,
    unfoldNote: manyAll.includes('data-acc="less"'),
  },
  payload: body,
  bare: bare,
}));`
	f, err := os.CreateTemp(t.TempDir(), "modellimits-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model limits node test failed: %v\n%s", err, out)
	}
	var got struct {
		Render struct {
			Manual       bool `json:"manual"`
			FreePill     bool `json:"freePill"`
			WarnBar      bool `json:"warnBar"`
			BadBar       bool `json:"badBar"`
			PartialTag   bool `json:"partialTag"`
			DegradingTag bool `json:"degradingTag"`
			Arrow        bool `json:"arrow"`
			FreeStep     bool `json:"freeStep"`
			ChainSource  bool `json:"chainSource"`
			NoChain      bool `json:"noChain"`
			EditBtn      bool `json:"editBtn"`
			RowKey       bool `json:"rowKey"`
			DetailTable  bool `json:"detailTable"`
			DetailUID    bool `json:"detailUid"`
			DetailKey    bool `json:"detailKey"`
			FoldCut      int  `json:"foldCut"`
			FoldNote     bool `json:"foldNote"`
			UnfoldRows   int  `json:"unfoldRows"`
			UnfoldNote   bool `json:"unfoldNote"`
		} `json:"render"`
		Payload struct {
			Realm                 string   `json:"realm"`
			Model                 string   `json:"model"`
			Source                string   `json:"source"`
			WindowSec             float64  `json:"window_sec"`
			TokenQuotaPerAccount  float64  `json:"token_quota_per_account"`
			CreditQuotaPerAccount float64  `json:"credit_quota_per_account"`
			CreditQuotaPerPool    float64  `json:"credit_quota_per_pool"`
			OnHit                 string   `json:"on_hit"`
			ChainMode             string   `json:"chain_mode"`
			Chain                 []string `json:"chain"`
			Note                  string   `json:"note"`
		} `json:"payload"`
		Bare struct {
			Source    string   `json:"source"`
			OnHit     string   `json:"on_hit"`
			ChainMode string   `json:"chain_mode"`
			WindowSec float64  `json:"window_sec"`
			Chain     []string `json:"chain"`
		} `json:"bare"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("解析渲染结果失败：%v\n%s", err, out)
	}
	r := got.Render
	for _, c := range []struct {
		name string
		got  bool
	}{
		{"口径标手动", r.Manual},
		{"零倍率标免费", r.FreePill},
		{"80% 以上进度条转黄", r.WarnBar},
		{"95% 以上进度条转红", r.BadBar},
		{"状态标签部分吃紧", r.PartialTag},
		{"降级中标签", r.DegradingTag},
		{"降级链用箭头串", r.Arrow},
		{"零倍率链步标免费", r.FreeStep},
		{"降级链带来源标签", r.ChainSource},
		{"空链写不降级", r.NoChain},
		{"行上有设限按钮", r.EditBtn},
		{"行带 realm|model 的 key", r.RowKey},
		{"明细是账号小表", r.DetailTable},
		{"明细带账号 uid", r.DetailUID},
		{"明细行带 data-det", r.DetailKey},
		{"超过 8 个账号默认折叠", r.FoldCut == 8},
		{"折叠时有展开全部按钮", r.FoldNote},
		{"展开后 10 个账号全给", r.UnfoldRows == 10},
		{"展开后有收起按钮", r.UnfoldNote},
	} {
		if !c.got {
			t.Errorf("%s：渲染结果不符合预期", c.name)
		}
	}
	p := got.Payload
	if p.Realm != "cn" || p.Model != "x" {
		t.Errorf("载荷 key 不对：realm=%q model=%q", p.Realm, p.Model)
	}
	if p.Source != "manual" || p.OnHit != "hold" || p.ChainMode != "zero" {
		t.Errorf("载荷口径不对：source=%q on_hit=%q chain_mode=%q", p.Source, p.OnHit, p.ChainMode)
	}
	if p.WindowSec != 7200 || p.TokenQuotaPerAccount != 1000000 || p.CreditQuotaPerAccount != 2.5 || p.CreditQuotaPerPool != 10 {
		t.Errorf("载荷配额不对：window=%v token=%v credit_acc=%v credit_pool=%v",
			p.WindowSec, p.TokenQuotaPerAccount, p.CreditQuotaPerAccount, p.CreditQuotaPerPool)
	}
	if strings.Join(p.Chain, ",") != "a,b,c,d" {
		t.Errorf("手动链没按逗号/换行拆开：%v", p.Chain)
	}
	if p.Note != "hi" {
		t.Errorf("备注没去空白：%q", p.Note)
	}
	if got.Bare.Source != "follow" || got.Bare.OnHit != "degrade" || got.Bare.ChainMode != "off" {
		t.Errorf("载荷默认值不对：source=%q on_hit=%q chain_mode=%q",
			got.Bare.Source, got.Bare.OnHit, got.Bare.ChainMode)
	}
	if got.Bare.WindowSec != 0 || len(got.Bare.Chain) != 0 {
		t.Errorf("载荷默认值不对：window=%v chain=%v", got.Bare.WindowSec, got.Bare.Chain)
	}
}

// TestModelLimitsResetWiring 「恢复默认」按钮的接线：按钮要在模型限流映射页的工具条里，
// 复位走已有的 policy 端点（不许开第 5 个端点），点之前必须能取消，
// 脚注还要写清 quota_reset_at 的口径（窗口边界 ≠ 额度到点恢复）。
func TestModelLimitsResetWiring(t *testing.T) {
	html := readModellimitsAsset(t, "index.html")
	js := readModellimitsAsset(t, "app.js")

	viewStart := strings.Index(html, "<!-- >>> modellimits-view")
	viewEnd := strings.Index(html, "<!-- <<< modellimits-view")
	if viewStart < 0 || viewEnd < viewStart {
		t.Fatalf("modellimits-view 标记块不成对（start=%d end=%d）", viewStart, viewEnd)
	}
	if !strings.Contains(html[viewStart:viewEnd], `id="btnMrlReset"`) {
		t.Error("模型限流映射页工具条缺少「恢复默认」按钮 #btnMrlReset")
	}

	blockStart := strings.Index(js, "// >>> modellimits-app")
	blockEnd := strings.Index(js, "// <<< modellimits-app")
	if blockStart < 0 || blockEnd < blockStart {
		t.Fatalf("modellimits-app 标记块不成对（start=%d end=%d）", blockStart, blockEnd)
	}
	block := js[blockStart:blockEnd]
	for _, want := range []string{
		"function resetMrlPolicy(",
		"JSON.stringify({ op: 'reset' })",
		"$('btnMrlReset').addEventListener('click'",
		"confirm(",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("app.js 的 modellimits 块缺少 %s", want)
		}
	}
	if n := strings.Count(block, "op: 'reset'"); n != 1 {
		t.Errorf("op=reset 出现 %d 次，应为 1 次", n)
	}
	// 复位按钮必须走 policy 端点（不是新开一个端点）。
	if !strings.Contains(block, "api('model_limits/policy'") {
		t.Error("恢复默认没有走 model_limits/policy 端点")
	}
	// 脚注口径说明：quota_reset_at 是「额度滚出窗口」的时刻，不是额度恢复时刻。
	if !strings.Contains(block, "整点小时桶") {
		t.Error("脚注缺少窗口口径说明（整点小时桶 / quota_reset_at 的含义）")
	}
}

// TestModelLimitsResetAndNewFieldsRender 后端新增字段的前端渲染：
// 行级 quota_reset_at（最早滚出）与 retry_after_sec（建议 X 后重试），
// 账号级 reason（上游原因）与 quota_reset_at（该账号的窗口边界）。
// 时间一律转本机 HH:MM，不把 RFC3339 原文甩到页面上。
func TestModelLimitsResetAndNewFieldsRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model limits new fields render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('// >>> modellimits-pure');
const end = src.indexOf('// <<< modellimits-pure');
if (start < 0 || end < start) throw new Error('modellimits pure region not found');
const ctx = {
  esc: s => String(s == null ? '' : s),
  formatTokenCount: n => (n == null ? '—' : String(n)),
};
vm.createContext(ctx);
vm.runInContext("'use strict';" +
  src.slice(start, end) +
  'this.limitRowHtml=limitRowHtml;this.limitDetailHtml=limitDetailHtml;', ctx);
const ISO = '2026-11-23T04:34:00Z';
const d = new Date(ISO);
const pad = n => String(n).padStart(2, '0');
const hhmm = pad(d.getHours()) + ':' + pad(d.getMinutes());
const row = {
  realm: 'cn', model: 'm1', full_name: 'cn:m1', rate: '1', has_rate: true, zero_rate: false,
  source: 'follow', window_sec: 86400,
  quota: { window_sec: 86400, token_per_account: 100, token_per_pool: 200, inferred: false },
  usage: { tokens: 10, requests: 2, errors: 0, credits: 0 },
  pool: { quota_tokens: 200, used_tokens: 195, remaining_tokens: 5, pct: 0.97 },
  accounts: [{ uid: 'u1', nickname: '小号', routable: true, limited: true, limit_kind: 'cooldown',
    reason: '上游并发超限', until: '2026-11-23T05:00:00Z', quota_reset_at: ISO,
    tokens: 100, remaining_tokens: 0, pct: 1, credits_left: 12 }],
  limit_state: 'locked', limited_accounts: 1, routable_accounts: 1,
  chain: [], chain_source: 'none', degrading: false,
  quota_reset_at: ISO, retry_after_sec: 45,
};
const plain = Object.assign({}, row, { quota_reset_at: '', retry_after_sec: 0 });
const html = ctx.limitRowHtml(row);
const plainHtml = ctx.limitRowHtml(plain);
const detail = ctx.limitDetailHtml(row, false);
const longHtml = ctx.limitRowHtml(Object.assign({}, row, { retry_after_sec: 5400 }));
process.stdout.write(JSON.stringify({
  rowReset: html.includes('最早滚出 ' + hhmm),
  rowRetry: html.includes('建议 45 秒后重试'),
  rowRetryLong: longHtml.includes('建议 1 小时 30 分后重试'),
  rawHidden: html.indexOf(ISO) < 0 && detail.indexOf(ISO) < 0,
  plainQuiet: plainHtml.indexOf('最早滚出') < 0 && plainHtml.indexOf('建议') < 0,
  accReason: detail.includes('上游并发超限'),
  accReset: detail.includes('额度滚出 ' + hhmm),
}));`
	f, err := os.CreateTemp(t.TempDir(), "modellimits-fields-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model limits new fields node test failed: %v\n%s", err, out)
	}
	var got struct {
		RowReset     bool `json:"rowReset"`
		RowRetry     bool `json:"rowRetry"`
		RowRetryLong bool `json:"rowRetryLong"`
		RawHidden    bool `json:"rawHidden"`
		PlainQuiet   bool `json:"plainQuiet"`
		AccReason    bool `json:"accReason"`
		AccReset     bool `json:"accReset"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("解析渲染结果失败：%v\n%s", err, out)
	}
	for _, c := range []struct {
		name string
		got  bool
	}{
		{"行上显示窗口边界（最早滚出 HH:MM）", got.RowReset},
		{"行上显示建议重试秒数", got.RowRetry},
		{"建议重试超过一小时换成人话", got.RowRetryLong},
		{"不把 RFC3339 原文渲染到页面", got.RawHidden},
		{"没有这两个字段时不显示噪音", got.PlainQuiet},
		{"账号明细显示上游限流原因", got.AccReason},
		{"账号明细显示该账号的窗口边界", got.AccReset},
	} {
		if !c.got {
			t.Errorf("%s：渲染结果不符合预期", c.name)
		}
	}
}
