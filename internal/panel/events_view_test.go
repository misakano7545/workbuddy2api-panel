package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestAppJSEventsView 事件日志页的三条硬约束：
//
//  1. 读取失败必须说出来。原来 loadEvents 用 .catch(() => ({entries: []})) 把 501
//     （归档未开启）和网络错误都变成「共 0 条事件」——和「真的没流量」长得一模一样，
//     外层 try/catch 因此成了死代码。
//  2. 表头 13 列与行内 13 个 <td> 必须一致，否则整张表错位。
//  3. 「推理强度」列要区分「客户端要的」与「网关补的/改写的」：只显示生效值时，
//     客户端请求会被网关默认档淹没（issue #43 同源歧义，见 eventEffortPill）。
//
// 三处都是纯前端、都看不见失败，所以用 node 跑真实函数而不是读源码断言。
func TestAppJSEventsView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; events view test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('async function loadEvents() {');
const e = src.indexOf('\n}\n', src.indexOf('function deriveEventExecutor(ua) {'));
if (s < 0 || e < 0) throw new Error('events view block not found');
const body = src.slice(s, e + 3);

function makeCtx(apiImpl) {
  const els = {};
  const el = (k) => (els[k] = els[k] || { textContent: '', value: '', innerHTML: '' });
  const ctx = {
    rawEventsData: [],
    esc: (x) => String(x == null ? '' : x),
    $: el,
    setHTML: (node, html) => { node.innerHTML = html; },
    api: apiImpl,
    renderEventTable: undefined,
  };
  vm.createContext(ctx);
  vm.runInContext('let rawEventsData = []; let eventRows = [];\n' + body +
    '\nthis.populate = populateEventFilterOptions; this.render = renderEventTable;' +
    ' this.load = loadEvents; this.get = () => rawEventsData;', ctx);
  return { ctx, els, el };
}

async function run() {
  const out = {};
  // 归档未开启：501 要说人话
  const a = makeCtx(async () => { const e = new Error('request logger not available'); e.status = 501; throw e; });
  a.el('eventFilterLimit').value = '100';
  await a.ctx.load();
  out.e501 = a.el('eventTableBody').innerHTML;
  out.badge501 = a.el('eventBadge').textContent;

  // 其它错误：把原始 message 带出来
  const b = makeCtx(async () => { const e = new Error('boom'); e.status = 500; throw e; });
  await b.ctx.load();
  out.e500 = b.el('eventTableBody').innerHTML;

  // 成功：三行事件，分别覆盖「老归档（无 requested 键）」「客户端没发档位、网关补默认档」
  // 「客户端请求与生效不一致（改写/降级）」三种推理强度展示；列数按 <td> 计。
  const c = makeCtx(async () => ({ entries: [
    {
      time: '2026-10-26T10:00:00Z', path: '/v1/chat/completions', model: 'wb-lite',
      account: 'a@b.c (nick)', status: 200, ok: true, duration_ms: 1200, ttfb_ms: 300,
      prompt_tokens: 10, completion_tokens: 20, total_tokens: 30,
      reasoning_tokens: 5, reasoning_effort: 'high', mode: 'stream',
      cache_hit_tokens: 4, cache_miss_tokens: 6, executor_type: 'CodexExecutor',
      client_ip: '10.0.0.1', user_agent: 'codex/1.0'
    },
    {
      time: '2026-10-26T10:01:00Z', path: '/v1/chat/completions', model: 'wb-lite',
      status: 200, ok: true, duration_ms: 1000, ttfb_ms: 200, mode: 'stream',
      reasoning_effort: 'max', reasoning_effort_requested: ''
    },
    {
      time: '2026-10-26T10:02:00Z', path: '/v1/chat/completions', model: 'wb-lite',
      status: 200, ok: true, duration_ms: 1000, ttfb_ms: 200, mode: 'stream',
      reasoning_effort: 'high', reasoning_effort_requested: 'max'
    }
  ] }));
  await c.ctx.load();
  const html = c.el('eventTableBody').innerHTML;
  out.rows = (html.match(/<tr[\s>]/g) || []).length; // 行带 data-ev 属性（上下文展开），不能再钉 <tr>
  out.cells = (html.match(/<td/g) || []).length / (out.rows || 1);
  out.hasFakeKey = /Gra\*+cle@1!/.test(html);
  out.hasUpstreamLie = html.indexOf('上游响应') >= 0;
  out.cacheMiss = html.indexOf('未命中 6') >= 0;
  out.oldEffort = html.indexOf('pill-effort">high<') >= 0; // 老归档：只显示生效值
  out.gwEffort = html.indexOf('-→max') >= 0;                // 客户端没发，网关补的默认档
  out.rwEffort = html.indexOf('max→high') >= 0;             // 客户端 max，实际发 high
  process.stdout.write(JSON.stringify(out));
}
run().catch(e => { console.error(e); process.exit(1); });`

	f, err := os.CreateTemp(t.TempDir(), "events-*.cjs")
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
	var got struct {
		E501           string  `json:"e501"`
		Badge501       string  `json:"badge501"`
		E500           string  `json:"e500"`
		Rows           int     `json:"rows"`
		Cells          float64 `json:"cells"`
		HasFakeKey     bool    `json:"hasFakeKey"`
		HasUpstreamLie bool    `json:"hasUpstreamLie"`
		CacheMiss      bool    `json:"cacheMiss"`
		OldEffort      bool    `json:"oldEffort"`
		GwEffort       bool    `json:"gwEffort"`
		RwEffort       bool    `json:"rwEffort"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if !strings.Contains(got.E501, "请求归档未开启") {
		t.Errorf("501 应当提示归档未开启，实际: %s", got.E501)
	}
	if got.Badge501 != "读取失败" {
		t.Errorf("失败时徽标不能留在旧数字，实际: %q", got.Badge501)
	}
	if !strings.Contains(got.E500, "读取失败: boom") {
		t.Errorf("其它错误要带出原始 message，实际: %s", got.E500)
	}
	for name, html := range map[string]string{"501": got.E501, "500": got.E500} {
		if strings.Contains(html, "共 0 条事件") || strings.Contains(html, "没有符合筛选条件") {
			t.Errorf("%s 被渲染成「没有事件」了: %s", name, html)
		}
	}
	if got.Rows != 3 {
		t.Fatalf("应当渲染 3 行，实际 %d", got.Rows)
	}
	if got.Cells != 13 {
		t.Errorf("行内列数应为 13（与表头一致），实际 %v", got.Cells)
	}
	if got.HasFakeKey || got.HasUpstreamLie {
		t.Errorf("伪造列回来了：假 key=%v 假上游模型=%v", got.HasFakeKey, got.HasUpstreamLie)
	}
	if !got.CacheMiss {
		t.Error("缓存未命中数没有渲染出来")
	}
	if !got.OldEffort {
		t.Error("升级前的老归档行（无 reasoning_effort_requested 键）应按旧口径只显示生效档位")
	}
	if !got.GwEffort {
		t.Error("客户端未携带档位时应显示「-→生效档」，不能只给裸 max（会让读者以为客户端要了 max）")
	}
	if !got.RwEffort {
		t.Error("客户端请求与生效档不一致时应显示「请求→生效」")
	}
}

// TestEventsTableHeaderColumns 表头列数与行内列数必须同步改。
func TestEventsTableHeaderColumns(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	i := strings.Index(string(raw), `<table class="event-table">`)
	if i < 0 {
		t.Fatal("事件表不存在")
	}
	seg := string(raw)[i:]
	if j := strings.Index(seg, "</thead>"); j > 0 {
		seg = seg[:j]
	}
	n := len(regexp.MustCompile(`<th[ >]`).FindAllString(seg, -1))
	if n != 13 {
		t.Errorf("事件表表头应为 13 列，实际 %d", n)
	}
}
