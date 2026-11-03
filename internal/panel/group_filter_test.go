package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAppJSGroupFilterAndBatchScope 分组筛选与「组内批量」的作用域（issue #159）。
//
// 为什么值得一测：组内批量是写操作（悬停一下就能把一批号撤出流量池）。若筛选没
// 真正生效、或批量谓词放宽成「全表」，界面看起来「只动了这一组」，实际动了整池——
// Go 侧全绿，JS 语法检查也发现不了。这里数真实渲染出来的行，而不是断言源码里有
// 某个字符串。
func TestAppJSGroupFilterAndBatchScope(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; group filter test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');

// ── 1) renderAccounts 的组筛选：只渲染选中组的行，lastVisibleAccounts 跟着收窄 ──
const s = src.indexOf('function renderAccounts('), e = src.indexOf('// renderModelLocks');
assert.ok(s >= 0 && e > s, 'renderAccounts block not found');
const tb = { innerHTML: '' };
const ctx = {
  String, Number, Math, Date, Object, Array, Set, JSON, RegExp, console,
  document: { activeElement: null },
  $: id => (id === 'accBody' ? tb : null),
  setHTML: (el, html) => { el.innerHTML = html; },
  esc: x => String(x == null ? '' : x),
  remarkName: (uid, html) => html, dur: () => '1m', ago: () => '刚刚',
  rateLimitRowsHtml: () => '', formatTokenCount: () => '0',
  formatLatency: () => '-', formatRate: () => '-', accountProxyHTML: () => '',
  syncGroupFilter: () => {},   // 下拉重建只碰 DOM，本测试关心筛选与批量作用域
  accRealmFilter: 'all', lastAccountsList: [],
  accGroupFilter: '', lastVisibleAccounts: [],
};
vm.createContext(ctx);
vm.runInContext(src.slice(s, e) + '\nthis.render = renderAccounts;', ctx);
const acct = (uid, group) => ({
  uid, group, realm: 'cn', credits: 1, nickname: uid,
  success_count: 1, err_total: 0, in_flight: 0, paused: false, disabled: false,
});
const rows = [acct('a1', 'A'), acct('a2', 'A'), acct('b1', 'B'), acct('c1', '')];

ctx.accGroupFilter = 'A';
ctx.render(rows);
assert.strictEqual(ctx.lastVisibleAccounts.length, 2, '组 A 应只剩 2 个账号');
assert.ok(!tb.innerHTML.includes('b1'), '组 A 的表格里不该出现 B 组账号');

ctx.accGroupFilter = '';
ctx.render(rows);
assert.strictEqual(ctx.lastVisibleAccounts.length, 4, '全部分组应显示全部 4 个');

// ── 2) 组内批量的谓词：不该动的账号一个都不许进目标集 ──
const gs = src.indexOf('const GROUP_ACT = {'), ge = src.indexOf('};', gs) + 2;
assert.ok(gs >= 0 && ge > gs, 'GROUP_ACT not found');
const bctx = {};
vm.createContext(bctx);
vm.runInContext(src.slice(gs, ge) + '\nthis.GROUP_ACT = GROUP_ACT;', bctx);
const G = bctx.GROUP_ACT;
assert.strictEqual(G.checkin.ok({}), true, '普通账号应参与组内签到');
assert.strictEqual(G.checkin.ok({ enterprise: true }), false, '企业版不该被组内签到');
assert.strictEqual(G.checkin.ok({ api_key: true }), false, '静态 Key 不该被组内签到');
assert.strictEqual(G.checkin.ok({ disabled: true }), false, '禁用号不该被组内签到');
assert.strictEqual(G.pause.ok({ paused: false }), true);
assert.strictEqual(G.pause.ok({ paused: true }), false, '已暂停的号不该再暂停一次');
assert.strictEqual(G.pause.ok({ disabled: true }), false, '禁用号不该走暂停');
assert.strictEqual(G.resume.ok({ paused: false }), false, '未暂停的号没有可恢复的');
assert.strictEqual(G.resume.ok({ paused: true }), true);
process.stdout.write('OK');
`
	dir := t.TempDir()
	f := filepath.Join(dir, "group-filter.cjs")
	if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, f, "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if string(out) != "OK" {
		t.Fatalf("unexpected output: %s", out)
	}
}
