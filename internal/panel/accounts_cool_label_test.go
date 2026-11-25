package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// TestAccountsCoolLabelUsesCoolKind 冷却标签必须照抄服务端的 cool_kind，不能比秒数。
//
// 为什么：cool_remaining_sec 是服务端 time.Until()+0.999 向上取整的**整秒**，而前端
// 的 bl 是拿到响应后再算的浮点秒 —— 只差几十毫秒，「只有熔断」时 bl 必然略小于
// cool_remaining_sec，`bl > cool_remaining_sec` 恒为 false，于是熔断被显示成
// 「限流冷却」（issue #290：11 个号被 11353 打进熔断，面板却说是上游限流，把排查
// 引向了错误方向）。state.go 已算出两者谁截止更晚并写进 cool_kind，前端照抄即可。
func TestAccountsCoolLabelUsesCoolKind(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; accounts cool label test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('function renderAccounts(list) {');
const e = src.indexOf('// renderModelLocks');
if (s < 0 || e < 0) throw new Error('renderAccounts block not found');

const stubs = {
  document: { activeElement: null },
  lastAccountsList: [], accRealmFilter: 'all',
  accGroupFilter: '', lastVisibleAccounts: [], syncGroupFilter: () => {},
  esc: x => String(x == null ? '' : x),
  remarkName: (uid, name) => name,
  rateLimitRowsHtml: () => '',
  disabledKind: () => null, disabledAge: () => '',
  ago: () => '刚刚', dur: () => '12m',
  formatTokenCount: () => '0', formatLatency: () => '-', formatRate: () => '-',
  accountProxyHTML: () => '',
  els: {},
};
const el = k => (stubs.els[k] = stubs.els[k] || { textContent: '', value: '', innerHTML: '', querySelectorAll: () => [] });
Object.assign(stubs, { $: el, setHTML: (node, html) => { node.innerHTML = html; } });
vm.createContext(stubs);
vm.runInContext('let lastAccountsList = []; let accRealmFilter = "all";\n' + src.slice(s, e) +
  '\nthis.render = renderAccounts;', stubs);

// 熔断截止时刻取「此刻 + 706s」，模拟服务端回 cool_remaining_sec=706 而前端后算
// 只有 705.x 秒的现场。
const breakerUntil = new Date(Date.now() + 706000).toISOString();
const acct = (uid, extra) => Object.assign({
  uid, nickname: uid, realm: 'cn', credits: 100, credits_total: 200,
  success_count: 1, err_total: 0, in_flight: 0,
}, extra || {});

function labels(html) {
  return [...html.matchAll(/<span class="tag warn">([^<]*)<\/span>/g)].map(m => m[1]);
}

const out = {};
stubs.render([acct('b1', { cool_remaining_sec: 706, cool_kind: 'breaker', breaker_until: breakerUntil })]);
out.breaker = labels(el('accBody').innerHTML);
stubs.render([acct('s1', { cool_remaining_sec: 60, cool_kind: 'soft_rate' })]);
out.soft = labels(el('accBody').innerHTML);
stubs.render([acct('h1', { cool_remaining_sec: 60, cool_kind: 'hard_credit' })]);
out.credit = labels(el('accBody').innerHTML);
process.stdout.write(JSON.stringify(out));`

	f, err := os.CreateTemp(t.TempDir(), "coollabel-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	raw, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
	var got struct {
		Breaker []string `json:"breaker"`
		Soft    []string `json:"soft"`
		Credit  []string `json:"credit"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, raw)
	}
	wantOne := func(name string, labels []string, prefix string) {
		t.Helper()
		if len(labels) != 1 {
			t.Errorf("%s: 应有 1 个 warn 标签，实得 %v", name, labels)
			return
		}
		if len(labels[0]) < len(prefix) || labels[0][:len(prefix)] != prefix {
			t.Errorf("%s: 标签 %q，应以 %q 开头", name, labels[0], prefix)
		}
	}
	wantOne("只有熔断", got.Breaker, "熔断")
	wantOne("只剩软冷却", got.Soft, "限流冷却")
	wantOne("积分冷却", got.Credit, "积分冷却")
}
