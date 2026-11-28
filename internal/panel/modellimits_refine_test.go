package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestModelLimitsPctAndOrder 百分比文本要如实显示超额（600M 用了 941.9M 写 157.0%），
// 进度条仍封顶 100%；行按池级已用额度降序（同已用按剩余升序）排序。
func TestModelLimitsPctAndOrder(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; percentage/order test skipped")
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
  'this.limitRowHtml=limitRowHtml;this.mrlSortRows=mrlSortRows;', ctx);
const over = ctx.limitRowHtml({
  realm: 'cn', model: 'm1', full_name: 'cn:m1', source: 'manual', has_rate: true, rate_value: 1,
  quota: { token_per_pool: 600000000 },
  usage: { tokens: 941900000 },
  pool: { quota_tokens: 600000000, used_tokens: 941900000, remaining_tokens: 0, pct: 941900000/600000000, credit_pct: 0 },
  accounts: [], limit_state: 'locked', limited_accounts: 0, routable_accounts: 0, chain: [], chain_source: 'none',
});
const rows = [
  { realm: 'cn', model: 'wide', full_name: 'cn:wide', source: 'follow', has_rate: true, rate_value: 1,
    pool: { remaining_tokens: 900000000, pct: 0.25, used_tokens: 300000000, quota_tokens: 1200000000 }, accounts: [] },
  { realm: 'cn', model: 'small', full_name: 'cn:small', source: 'follow', has_rate: true, rate_value: 1,
    pool: { remaining_tokens: 0, pct: 2, used_tokens: 400000000, quota_tokens: 200000000 }, accounts: [] },
  { realm: 'cn', model: 'mid', full_name: 'cn:mid', source: 'follow', has_rate: true, rate_value: 1,
    pool: { remaining_tokens: 100000000, pct: 0.92, used_tokens: 1100000000, quota_tokens: 1200000000 }, accounts: [] },
];
const order = ctx.mrlSortRows(rows).map(r => r.model);
process.stdout.write(JSON.stringify({
  overText: over.includes('157.0%'),
  noCapText: !/已用[^<]*·\s*100\.0%/.test(over),
  barCapped: over.includes('ml-bar bad'),
  order: order.join(','),
}));`
	f, err := os.CreateTemp(t.TempDir(), "modellimits-pct-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(script)
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model limits pct node test failed: %v\n%s", err, out)
	}
	var got struct {
		OverText  bool   `json:"overText"`
		NoCapText bool   `json:"noCapText"`
		BarCapped bool   `json:"barCapped"`
		Order     string `json:"order"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("解析渲染结果失败：%v\n%s", err, out)
	}
	if !got.OverText || !got.NoCapText {
		t.Errorf("超额百分比应显示真实值 157.0%%：%+v", got)
	}
	if !got.BarCapped {
		t.Error("超额时进度条仍应是 bad 样式（视觉封顶 100%）")
	}
	// 池级已用额度降序：mid 1.1B > small 400M（只有 200M 配额，超额）> wide 300M。
	if got.Order != "mid,small,wide" {
		t.Errorf("前端排序应按池级已用额度降序：got %q", got.Order)
	}
}
