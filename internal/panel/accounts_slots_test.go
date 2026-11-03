package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// TestAccountsActsSlots 账号池操作列的槽位数：没有冻结账号时不能给「探活」留空列。
//
// 为什么需要：八个动作按固定槽位输出（缺位留空 span），整列才能逐行对齐。「探活」是
// 冻结态专属，如果无条件占位，29 个健康账号每行都在「任务」和「暂停选号」之间空出
// 一条 64px 的缝（用户反馈「中间留白难受」）。反过来也不能直接删——有冻结行时少一列，
// 8 个孩子塞进 7 个 grid 轨道会折出隐式第二行，按钮整体错位。两条都看不见失败，所以
// 用 node 跑真实 renderAccounts 数槽位。
func TestAccountsActsSlots(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; accounts slots test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('function renderAccounts(list) {');
const e = src.indexOf('// renderModelLocks');
if (s < 0 || e < 0) throw new Error('renderAccounts block not found');

// 只测操作列槽位，其余渲染依赖全部打桩。
const stubs = {
  document: { activeElement: null },
  lastAccountsList: [], accRealmFilter: 'all',
  accGroupFilter: '', lastVisibleAccounts: [], syncGroupFilter: () => {},
  esc: x => String(x == null ? '' : x),
  remarkName: (uid, name) => name,
  rateLimitRowsHtml: () => '',
  disabledKind: () => null, disabledAge: () => '',
  ago: () => '刚刚', dur: () => '1m',
  formatTokenCount: () => '0', formatLatency: () => '-', formatRate: () => '-',
  accountProxyHTML: () => '',
  els: {},
};
const el = k => (stubs.els[k] = stubs.els[k] || { textContent: '', value: '', innerHTML: '', querySelectorAll: () => [] });
Object.assign(stubs, { $: el, setHTML: (node, html) => { node.innerHTML = html; } });
vm.createContext(stubs);
vm.runInContext('let lastAccountsList = []; let accRealmFilter = "all";\n' + src.slice(s, e) +
  '\nthis.render = renderAccounts;', stubs);

const acct = (uid, extra) => Object.assign({
  uid, nickname: uid, realm: 'cn', credits: 100, credits_total: 200,
  success_count: 1, err_total: 0, in_flight: 0,
}, extra || {});

// 操作列内每个槽位（按钮或空 span）算一个
function slots(html) {
  const out = [];
  const re = /<div class="acts-inner([^"]*)">([\s\S]*?)<\/div>/g;
  let m;
  while ((m = re.exec(html))) {
    const seg = m[2];
    out.push({
      cls: m[1].trim(),
      n: (seg.match(/<button|<span class="slot"/g) || []).length,
      probe: seg.indexOf('data-a="probe_ban"') >= 0,
    });
  }
  return out;
}

const out = {};
// 三个健康账号：不该有探活 & 不该有 .with-probe
stubs.render([acct('u1'), acct('u2'), acct('u3')]);
out.healthy = slots(el('accBody').innerHTML);
// 一个禁用 + 一个冷却中的普通账号
stubs.render([acct('u1'), acct('u2', { disabled: true }), acct('u3', { cool_remaining_sec: 60 })]);
out.frozen = slots(el('accBody').innerHTML);
process.stdout.write(JSON.stringify(out));`

	f, err := os.CreateTemp(t.TempDir(), "accslots-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	type row struct {
		Cls   string `json:"cls"`
		N     int    `json:"n"`
		Probe bool   `json:"probe"`
	}
	var got struct {
		Healthy []row `json:"healthy"`
		Frozen  []row `json:"frozen"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}

	if len(got.Healthy) != 3 {
		t.Fatalf("健康池应渲染 3 行，实际 %d", len(got.Healthy))
	}
	for i, r := range got.Healthy {
		if r.Cls != "" {
			t.Errorf("健康池不该带 with-probe（第 %d 行: %q）", i+1, r.Cls)
		}
		if r.N != 7 {
			t.Errorf("健康行槽位应为 7（签到/余额/任务/选号/禁用/备注/移除），第 %d 行实际 %d", i+1, r.N)
		}
		if r.Probe {
			t.Errorf("健康账号不该出现探活按钮（第 %d 行）", i+1)
		}
	}

	if len(got.Frozen) != 3 {
		t.Fatalf("含冻结账号的池应渲染 3 行，实际 %d", len(got.Frozen))
	}
	for i, r := range got.Frozen {
		if r.Cls != "with-probe" {
			t.Errorf("有冻结账号时整表都要带 with-probe（第 %d 行: %q）", i+1, r.Cls)
		}
		if r.N != 8 {
			t.Errorf("含冻结账号时每行槽位应为 8，第 %d 行实际 %d（少一个会让 grid 折行、整列错位）", i+1, r.N)
		}
	}
	if got.Frozen[1].Probe == false {
		t.Error("禁用账号必须渲染探活按钮")
	}
	if got.Frozen[2].Probe == false {
		t.Error("冷却中的账号也必须渲染探活按钮")
	}
	if got.Frozen[0].Probe {
		t.Error("健康账号在有冻结邻居时也不该自己渲染探活")
	}
}

// TestActsInnerGridTracks CSS 的轨道数必须和上面对得上：默认 7 列，with-probe 8 列。
func TestActsInnerGridTracks(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	base := regexp.MustCompile(`\.acts-inner \{ display: grid; grid-template-columns: ([^;]+);`).
		FindStringSubmatch(string(raw))
	if base == nil {
		t.Fatal(".acts-inner grid 规则不见了")
	}
	if base[1] != "repeat(3, 54px) 68px repeat(3, 54px)" {
		t.Errorf("默认轨道应为 7 个槽（签到/余额/任务/选号 68px/禁用/备注/移除），实际: %s", base[1])
	}
	with := regexp.MustCompile(`\.acts-inner\.with-probe \{ grid-template-columns: ([^;]+);`).
		FindStringSubmatch(string(raw))
	if with == nil {
		t.Fatal(".acts-inner.with-probe 规则不见了 —— 有冻结账号时会缺一列")
	}
	if with[1] != "repeat(4, 54px) 68px repeat(3, 54px)" {
		t.Errorf("with-probe 应为 8 个槽（多一个探活），实际: %s", with[1])
	}
}
