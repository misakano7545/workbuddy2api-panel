package panel

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// 积分扣除维度的格式必须稳定，且缺样本/缺匹配 Token 时不能伪造比例。
func TestAppJSCreditDimensionFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
	const start = src.indexOf('function trimFixed');
const end = src.indexOf('function usStat');
if (start < 0 || end < 0) throw new Error('credit helpers not found');
const ctx = { Number, String, RegExp };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.fmtCredit=fmtCredit; this.fmtCreditRatio=fmtCreditRatio; this.fmtModelRate=fmtModelRate;',
  ctx
);
process.stdout.write(JSON.stringify({
  credit: ctx.fmtCredit(1.25),
  zero: ctx.fmtCredit(0),
  hundred: ctx.fmtCredit(100),
  ratio: ctx.fmtCreditRatio(12.5, 2, 400),
  noSamples: ctx.fmtCreditRatio(12.5, 0, 400),
  noTokens: ctx.fmtCreditRatio(12.5, 2, 0),
  rate: ctx.fmtModelRate('0.5'),
  noRate: ctx.fmtModelRate(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"credit":"1.25","zero":"0","hundred":"100","ratio":"12.5 / 1M","noSamples":"—","noTokens":"—","rate":"x0.5","noRate":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit formatting=%s want %s", out, want)
	}
}

// 日志行分级不得把「空赋值字段」当失败：`claim_error=""`（值为空 = 该项没有错误）
// 里带 error 字样，裸匹配会让成功的任务行（status=done claimed=true）整行标红。
func TestAppJSLogLevelNoFalsePositive(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; log level test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function logLevel');
const end = src.indexOf('async function loadLogs');
if (start < 0 || end < 0) throw new Error('logLevel not found');
const ctx = { String, RegExp };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.logLevel=logLevel;', ctx);
process.stdout.write(JSON.stringify({
  okTask: ctx.logLevel('panel: task auto uid=df6b07f7 code=Sequential_Tasks_6 status=done progress=10/10->10/10 claimed=true claim_error=""'),
  twoEmpty: ctx.logLevel('panel: task auto uid=x status=done claim_error="" balance_error=""'),
  realFail: ctx.logLevel('panel: task auto uid=x status=awaiting_progress claimed=false claim_error="task not completed"'),
  realBalanceErr: ctx.logLevel('panel: task auto uid=x status=done claim_error="" balance_error="timeout"'),
  chineseFail: ctx.logLevel('panel: disable uid=x（人工禁用）失败'),
  warn: ctx.logLevel('pool: uid=x 冷却中'),
  plain: ctx.logLevel('panel: task auto uid=x status=done'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "log-level-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("log level node test failed: %v\n%s", err, out)
	}
	const want = `{"okTask":"","twoEmpty":"","realFail":" e","realBalanceErr":" e","chineseFail":" e","warn":" w","plain":""}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("log level=%s want %s", out, want)
	}
}

// 任务中心合并后的两条易回归点：①后台任务 → 与队列同形的分组（空结果不占位）；
// ②归属标志：队列/扫描结果占着列表时，后台任务的刷新不得把它冲掉。
func TestAppJSTaskCenterMerge(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; task center merge test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function taskOutcomeStatus');
const end = src.indexOf("$('btnTaskJobsReload')");
if (start < 0 || end < 0) throw new Error('task center helpers not found');
const calls = [];
const store = { jobs: [
  { uid: 'u1', nickname: 'A', trigger: 'daily', status: 'finished', completed: 2, total: 2, results: [
    { task_code: 'T1', title: 't1', status: 'done', progress_after: '1/1' },
    { task_code: 'T2', title: 't2', claim_error: 'task not completed' },
  ]},
  { uid: 'u2', nickname: 'B', trigger: 'new_account', status: 'running', results: [] },
] };
const ctx = {
  String, Number, RegExp, Object, Array, Map, JSON, Promise, console,
  api: async () => ({ jobs: store.jobs, auto_enabled: true }),
  renderQueue: (groups, progress, title, desc) => calls.push({ groups, title, desc }),
  $: () => ({ textContent: '', hidden: false, style: {}, querySelector: () => ({ textContent: '' }), addEventListener() {} }),
  esc: s => String(s == null ? '' : s),
  ago: () => '刚刚',
  qrowHTML: () => '',
};
vm.createContext(ctx);
vm.runInContext('let taskJobsLoading = false;\n' + src.slice(start, end) +
  '\nthis.loadTaskJobs = loadTaskJobs; this.groupsFromJobs = groupsFromJobs; this.setOwner = v => { taskCenterOwner = v; };', ctx);
ctx.setJobs = v => { store.jobs = v; };
(async () => {
  // ① 分组 join：空结果的账号不占位，行形态与队列一致（code/status/prog）
  const groups = ctx.groupsFromJobs([
    { uid: 'u1', nickname: 'A', trigger: 'daily', status: 'finished', completed: 2, total: 2, results: [
      { task_code: 'T1', title: 't1', status: 'done', progress_after: '1/1' },
      { task_code: 'T2', title: 't2', claim_error: 'task not completed' },
    ]},
    { uid: 'u2', nickname: 'B', trigger: 'new_account', status: 'running', results: [] },
  ]);
  // ② 归属：队列占着列表时后台刷新只更新提示、不渲染
  ctx.setOwner('queue');
  await ctx.loadTaskJobs();
  const queueSkip = calls.length === 0;
  ctx.setOwner('jobs');
  await ctx.loadTaskJobs();
  const withJobs = calls[0].groups.length;
  // ③ 没有后台任务时给空态（不是留一张空列表）
  ctx.setJobs([]);
  await ctx.loadTaskJobs();
  process.stdout.write(JSON.stringify({
    groups: groups.length,
    rows: groups[0] ? groups[0].rows.map(r => [r.code, r.status, r.prog]) : null,
    cnt: groups[0] ? groups[0].cnt : '',
    queueSkip,
    withJobs,
    emptyGroups: calls[1].groups.length,
    emptyTitle: calls[1].title,
  }));
})();`
	f, err := os.CreateTemp(t.TempDir(), "task-center-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("task center node test failed: %v\n%s", err, out)
	}
	const want = `{"groups":1,"rows":[["T1","done","1/1"],["T2","claim_pending",""]],` +
		`"cnt":"每日定时 · 已结束 · 处理 2 / 2 项","queueSkip":true,"withJobs":1,` +
		`"emptyGroups":0,"emptyTitle":"还没有后台任务记录"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("task center merge=%s\nwant %s", out, want)
	}
}

// 成长任务行第二列必须是中文名，不能退回左边那列代号。
//
// 为什么需要：后台任务结果行只存 task_code + desc（线上实测 24 个任务码全无 title），
// 而 title 兜底链原本是 title → GROWTH_TITLES → code，扫描没跑过就退成代号，
// 出现「Model_chat_GLM5.2  Model_chat_GLM5.2」两列同名。
func TestAppJSTaskRowNameFallback(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; task row name test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const jobsStart = src.indexOf('function taskOutcomeStatus'), jobsEnd = src.indexOf("$('btnTaskJobsReload')");
const nameStart = src.indexOf('function groupItems(d)'), nameEnd = src.indexOf('function renderQueue');
const titlesStart = src.indexOf('const GROWTH_TITLES'), titlesLine = src.slice(titlesStart, src.indexOf('\n', titlesStart));
assert.ok(jobsStart >= 0 && jobsEnd > jobsStart && nameStart >= 0 && nameEnd > nameStart && titlesStart >= 0);
const ctx = { String, Number, RegExp, Object, Array, Map, JSON, console, esc: s => String(s == null ? '' : s) };
const dom = new Proxy(function () {}, { get: (t, k) => (k === Symbol.toPrimitive ? () => '' : dom), set: () => true, apply: () => dom });
Object.assign(ctx, { $: () => dom, document: dom, window: dom, localStorage: dom, location: dom });
vm.createContext(ctx);
vm.runInContext(titlesLine + '\n' + src.slice(nameStart, nameEnd) + '\n' + src.slice(jobsStart, jobsEnd) +
  '\nthis.GROWTH_TITLES = GROWTH_TITLES;', ctx);
// ① 只有 desc（后台任务行的真实形状）：第二列出中文说明，代号只出现一次
const desc = '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报';
const html = ctx.qrowHTML({ kind: 'job', code: 'Model_chat_GLM5.2', desc, status: 'done', prog: '1/1' });
assert.ok(html.includes(desc), 'name column should show desc: ' + html);
assert.equal(html.split('Model_chat_GLM5.2').length - 1, 1, 'code must appear once only: ' + html);
// ② 扫描缓存里的上游短名优先于 desc
ctx.GROWTH_TITLES['RichMeow_Chat'] = '桌面端对话1次';
assert.ok(ctx.qrowHTML({ code: 'RichMeow_Chat', desc: 'x', status: 'done' }).includes('桌面端对话1次'));
// ③ 两者都没有才退回代号（上游未知码的兜底不变）
assert.ok(ctx.qrowHTML({ code: 'unknown_code', status: 'done' }).includes('>unknown_code<'));
// ④ groupsFromJobs 必须把 desc 透传给行
const groups = ctx.groupsFromJobs([{ uid: 'u1', nickname: 'A', trigger: 'daily', status: 'finished',
  completed: 1, total: 1, results: [{ task_code: 'Model_chat_GLM5.2', desc, status: 'done' }] }]);
assert.equal(groups[0].rows[0].desc, desc);
assert.ok(ctx.qrowHTML(groups[0].rows[0]).includes(desc));
console.log('task row name fallback passed');`
	path := filepath.Join(t.TempDir(), "task-row-name.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// 模型限流时间必须同时支持上游 reset_at、网关 until 和无重置时间三种形态。
func TestAppJSRateLimitMeta(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate limit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function dur(');
const end = src.indexOf('function rateLimitRowsHtml');
if (start < 0 || end < 0) throw new Error('rate limit helpers not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rateLimitMeta=rateLimitMeta;', ctx);
const now = new Date(2026, 8, 28, 14, 0, 0).getTime();
const reset = new Date(2026, 8, 28, 16, 0, 0).getTime();
const until = new Date(2026, 8, 28, 15, 0, 0).getTime();
// capped：上游文案（16:00）晚于网关封顶解锁（15:00）——两个时刻都要写，剩余跟 until。
const capped = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(reset).toISOString(), until: new Date(until).toISOString() }, now);
// onlyUntil：上游没给重置文案，只有网关解锁时刻。
const onlyUntil = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', until: new Date(until).toISOString() }, now);
// equal：上游文案与网关解锁同一时刻（服务端会省略 reset_at，这里也须只显示一个时间）。
const equal = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(until).toISOString(), until: new Date(until).toISOString() }, now);
const unavailable = ctx.rateLimitMeta({ model: 'missing', kind: 'model_unavailable', until: new Date(until).toISOString() }, now);
const unknown = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit' }, now);
process.stdout.write(JSON.stringify({
  capped: capped.detail,
  onlyUntil: onlyUntil.detail,
  equal: equal.detail,
  unavailable: unavailable.detail,
  unknown: unknown.detail,
}));`
	f, err := os.CreateTemp(t.TempDir(), "rate-limit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("rate-limit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"capped":"上游称 2026-09-28 16:00 恢复 · 网关 2026-09-28 15:00 起重试（剩余 1时00分）","onlyUntil":"预计 2026-09-28 15:00 恢复（剩余 1时00分）","equal":"预计 2026-09-28 15:00 解封（剩余 1时00分）","unavailable":"预计 1时00分 后重试","unknown":"预计解封时间未知"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate-limit formatting=%s want %s", out, want)
	}
}

// 请求记录行必须紧凑、可读，并带上调用来源（IP / UA）；来源缺失时以 — 兜底。
func TestAppJSRequestLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.requestLogText=requestLogText;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const good = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0' };
const noSource = { ...good, request_id: 'req-3', client_ip: '', user_agent: '' };
const cached = { ...good, request_id: 'req-2', cache_hit_tokens: 2257, cache_miss_tokens: 43 };
process.stdout.write(JSON.stringify({
  good: ctx.requestLogText(good),
  noSource: ctx.requestLogText(noSource),
  cached: ctx.requestLogText(cached),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-log-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request log formatting node test failed: %v\n%s", err, out)
	}
	text := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-1"
	noSource := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | — | — | 1.25s | 2.3k tok | 0.12 credit | req-3"
	cached := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | 命中 98.1% | req-2"
	want := `{"good":` + strconv.Quote(text) + `,"noSource":` + strconv.Quote(noSource) + `,"cached":` + strconv.Quote(cached) + `}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request log formatting=%s want %s", out, want)
	}
}

// 积分历史区块：变动列 +N / −N（U+2212）、余额千分位、空列表提示。
func TestAppJSCreditHistoryFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit history formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function creditNum');
const end = src.indexOf('function renderCreditHistory');
if (start < 0 || end < 0) throw new Error('credit history helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.creditEntryText=creditEntryText;this.creditHistoryNote=creditHistoryNote;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
process.stdout.write(JSON.stringify({
  gain: ctx.creditEntryText({ time, uid: '89374120', account: '小明', delta: 100, before: 1300, after: 1400 }),
  loss: ctx.creditEntryText({ time, uid: '89374120', account: '', delta: -30, before: 1400, after: 1370 }),
  note: ctx.creditHistoryNote([{ delta: 1300 }, { delta: -60 }]),
  empty: ctx.creditHistoryNote([]),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-history-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit history formatting node test failed: %v\n%s", err, out)
	}
	gain := "14:05:06 | 小明 | +100 | 1,400 | 余额 1,300 → 1,400"
	loss := "14:05:06 | 89374120 | −30 | 1,370 | 余额 1,400 → 1,370"
	want := `{"gain":` + strconv.Quote(gain) + `,"loss":` + strconv.Quote(loss) +
		`,"note":"2 条 · 净 +1,240","empty":"暂无积分变动记录"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit history formatting=%s want %s", out, want)
	}
}

// 请求记录筛选：IP / UA / 模型 / 账号 / 请求 ID 的包含匹配（空格分词 AND）+ 结果精确匹配。
func TestAppJSRequestMatch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function reqMatch');
const end = src.indexOf('function reqOutcomeTag');
if (start < 0 || end < 0) throw new Error('reqMatch not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.reqMatch=reqMatch;', ctx);
const base = { outcome: 'success', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.3', account: '示例(uid8)', request_id: 'req-1' };
const other = { outcome: 'http_error', client_ip: '198.51.100.4', user_agent: 'Mozilla/5.0 Chrome/120', model: 'global:hy3', account: '甲(uid9)', request_id: 'req-2' };
const rows = [base, other];
const pick = f => rows.filter(e => ctx.reqMatch(e, f)).map(e => e.request_id);
process.stdout.write(JSON.stringify({
  all: pick({ q: '', outcome: '' }),
  byIP: pick({ q: '203.0.113', outcome: '' }),
  byUA: pick({ q: 'chrome/120', outcome: '' }),
  byModel: pick({ q: 'glm', outcome: '' }),
  multiKw: pick({ q: 'glm success', outcome: '' }),
  multiMiss: pick({ q: 'glm chrome', outcome: '' }),
  byOutcome: pick({ q: '', outcome: 'http_error' }),
  combined: pick({ q: '198.51', outcome: 'http_error' }),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request filter node test failed: %v\n%s", err, out)
	}
	// q 对 outcome 不参与匹配（outcome 有独立下拉），multiKw 里的 success 命中不了任何字段。
	const want = `{"all":["req-1","req-2"],"byIP":["req-1"],"byUA":["req-2"],"byModel":["req-1"],"multiKw":[],"multiMiss":[],"byOutcome":["req-2"],"combined":["req-2"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request filter=%s want %s", out, want)
	}
}

// 模型按条件查询：域 / 能力 / 档位 / 价格 / 关键词，以及倍率、上下文、输出排序。
func TestAppJSModelFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdRateValue');
const end = src.indexOf('function renderModels');
if (start < 0 || end < 0) throw new Error('model filter helpers not found');
// rateCell/outCell 定义在切片之外（各管一个单元格），本测试只关心行内徽标/档位列，桩掉。
const ctx = { Number, String, Array, Object, isFinite, parseFloat, esc: v => String(v == null ? '' : v), rateCell: () => '', outCell: () => '' };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdMatch=mdMatch; this.mdSortList=mdSortList; this.mdRateValue=mdRateValue; this.mdRowHtml=mdRowHtml;', ctx);
const models = [
  { id: 'cn:glm-5.2', name: 'GLM-5.2', vendor: 'Zhipu', tags: ['视觉'], supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: true, supported_efforts: ['high', 'xhigh'], default_effort: 'high', is_default: false, credits: '0.79', promo_factor: 0.5, promo_credits: '0.40', promo_label: '夜间折扣', context_length: 1000000, max_output_tokens: 131000 },
  { id: 'cn:hy3', name: 'Hy3', supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: false, supported_efforts: ['low', 'high'], default_effort: 'high', is_default: false, credits: '0', promo_factor: 0, promo_credits: '0', promo_label: '限时免费', context_length: 192000, max_output_tokens: 64000 },
  { id: 'global:hy3', name: 'Hy3 Global', supports_tool_call: false, supports_images: false, supports_reasoning: false, supported_efforts: [], is_default: false, credits: '0.11', context_length: 1000000, max_output_tokens: 393000 },
  { id: 'cn:auto', name: 'Auto', supports_tool_call: true, supports_images: true, supports_reasoning: true, is_default: true, credits: null, context_length: 256000, max_output_tokens: 32000 },
  // 出图模型：目录里没有思考档位与上下文，行内要标「出图」而不是「不支持思考」。
  { id: 'cn:hunyuan-image-alpha', name: 'Hunyuan Image Alpha', tags: ['text-to-image'], image_generation: true, credits: '5.00' },
];
const ids = list => list.map(m => m.id);
const filter = f => ids(ctx.mdSortList(models.filter(m => ctx.mdMatch(m, f)), f));
process.stdout.write(JSON.stringify({
  all: ids(models),
  realm: filter({ realm: 'cn' }),
  tool: filter({ cap: 'tool' }),
  vision: filter({ cap: 'vision' }),
  reasoning: filter({ cap: 'reasoning' }),
  isDefault: filter({ cap: 'default' }),
  effortOff: filter({ effort: 'off' }),
  effortLow: filter({ effort: 'low' }),
  free: filter({ promo: 'free' }),
  promo: filter({ promo: 'promo' }),
  discount: filter({ promo: 'discount' }),
  q: filter({ q: 'glm zhipu' }),
  qMiss: filter({ q: 'glm nosuch' }),
  sortRate: filter({ sort: 'rate' }),
  sortContext: filter({ sort: 'context' }),
  sortOutput: filter({ sort: 'output' }),
  sortName: filter({ sort: 'name' }),
  imgBadge: /出图/.test(ctx.mdRowHtml(models[4], null)),
  imgById: filter({ q: 'hunyuan-image-alpha' }),
  rateFree: ctx.mdRateValue(models[1]),
  rateMissing: ctx.mdRateValue(models[3]),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model filter node test failed: %v\n%s", err, out)
	}
	const want = `{"all":["cn:glm-5.2","cn:hy3","global:hy3","cn:auto","cn:hunyuan-image-alpha"],` +
		`"realm":["cn:glm-5.2","cn:hy3","cn:auto","cn:hunyuan-image-alpha"],` +
		`"tool":["cn:glm-5.2","cn:hy3","cn:auto"],"vision":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"reasoning":["cn:glm-5.2","cn:hy3","cn:auto"],"isDefault":["cn:auto"],` +
		`"effortOff":["cn:glm-5.2"],"effortLow":["cn:hy3"],"free":["cn:hy3"],` +
		`"promo":["cn:glm-5.2","cn:hy3"],"discount":["cn:glm-5.2"],"q":["cn:glm-5.2"],"qMiss":[],` +
		`"sortRate":["cn:hy3","global:hy3","cn:glm-5.2","cn:hunyuan-image-alpha","cn:auto"],` +
		`"sortContext":["cn:glm-5.2","global:hy3","cn:auto","cn:hy3","cn:hunyuan-image-alpha"],` +
		`"sortOutput":["global:hy3","cn:glm-5.2","cn:hy3","cn:auto","cn:hunyuan-image-alpha"],` +
		`"sortName":["cn:auto","cn:glm-5.2","cn:hunyuan-image-alpha","cn:hy3","global:hy3"],` +
		`"imgBadge":true,"imgById":["cn:hunyuan-image-alpha"],"rateFree":0,"rateMissing":null}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model filter=%s\nwant %s", out, want)
	}
}

// 用量时序图的柱体类名不得叫 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
// height 是 rect 的 CSS 几何属性——同名类会把每根柱子压成 3px 高，图看起来"没数据"。
// 这个坑只能在浏览器里看出来，所以在这里钉住类名。
func TestAppJSUsageChartBarClass(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const end = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
if ([start, end, escStart, escEnd, fmtStart, fmtEnd].some(v => v < 0)) throw new Error('usage chart helpers not found');
const host = { innerHTML: '', textContent: '' };
const ctx = {
  Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity,
  document: { getElementById: () => host },
  $: () => host,
};
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
  '\nthis.renderUsageChart=renderUsageChart;', ctx);
const series = [
  { t: '2026-09-30T09', scope: 'hour', prompt_tokens: 35, completion_tokens: 16, total_tokens: 51, requests: 1 },
  { t: '2026-09-30T11', scope: 'hour', prompt_tokens: 978324, completion_tokens: 20621, total_tokens: 998945, requests: 39 },
  { t: '2026-09-30T13', scope: 'hour', prompt_tokens: 27400952, completion_tokens: 104913, total_tokens: 27505865, requests: 200 },
];
ctx.renderUsageChart(series);
const svg = host.innerHTML;
process.stdout.write(JSON.stringify({
  hasUsbar: svg.includes('class="usbar"'),
  hasBareBar: /class="bar"/.test(svg),
  hasGradient: svg.includes('usGradP') && svg.includes('usGradC'),
  barCount: (svg.match(/class="usbar"/g) || []).length,
  hasPeak: svg.includes('峰值'),
  hasAvg: svg.includes('均值'),
  // 每根柱子一条自己的 <title>，且每条都是该点的数据（悬停不再全部显示最后一条）。
  barTitles: (svg.match(/<g><title>/g) || []).length,
  bareTitles: (svg.match(/<title>/g) || []).length - (svg.match(/<g><title>/g) || []).length,
  titleHasOwnReq: (function () {
    const ts = [...svg.matchAll(/<g><title>([^<]*)<\/title>/g)].map(m => m[1]);
    return ts.length === 3 && ts.some(x => x.includes('1 次')) && ts.some(x => x.includes('39 次')) && ts.some(x => x.includes('200 次'));
  })(),
  emptyState: (function () { ctx.renderUsageChart([]); return host.innerHTML.includes('us-empty'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "usage-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("usage chart node test failed: %v\n%s", err, out)
	}
	const want = `{"hasUsbar":true,"hasBareBar":false,"hasGradient":true,"barCount":6,"hasPeak":true,"hasAvg":true,` +
		`"barTitles":3,"bareTitles":0,"titleHasOwnReq":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("usage chart=%s\nwant %s", out, want)
	}
}

// 时间范围控件：预设 → 查询参数的映射。要点：
//   - 「今天」必须发浏览器本地时区的 00:00（服务端时区未必一致），且不带 to；
//   - 滚动预设 rolling=true 发 hours（服务端整点对齐），rolling=false 折算成 from；
//   - 「全部历史」两者都不发；「自定义」发用户挑的 from/to。
func TestAppJSTimeRangeQuery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; time range test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const TRANGE_PRESETS');
const end = src.indexOf('function rateLimitMeta');
if (start < 0 || end < 0 || end < start) throw new Error('trange helpers not found');
const host = { innerHTML: '' };
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams,
  document: { getElementById: () => host },
  $: () => host,
  esc: s => String(s == null ? '' : s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.trangeState=trangeState; this.trangeQuery=trangeQuery; this.trangeLabel=trangeLabel; this.trangeMidnight=trangeMidnight;', ctx);
const q = (preset, rolling) => {
  ctx.trangeState('t').preset = preset;
  return ctx.trangeQuery('t', rolling).toString();
};
const secOf = d => String(Math.floor(d.getTime() / 1000));
const approx = (qs, wantSec) => {
  const m = /(?:^|&)from=(\d+)/.exec(qs);
  return m && Math.abs(Number(m[1]) - wantSec) < 120;
};
const now = Date.now();
const todayQ = q('today', true);
process.stdout.write(JSON.stringify({
  todayIsMidnight: todayQ === 'from=' + secOf(ctx.trangeMidnight()),
  todayNoTo: !/to=/.test(todayQ),
  rolling24: q('24', true),
  rolling72: q('72', true),
  rolling0: q('0', true),
  log24From: approx(q('24', false), Math.floor((now - 24 * 3600e3) / 1000)),
  log24HasHours: /hours=/.test(q('24', false)),
  log7dFrom: approx(q('168', false), Math.floor((now - 168 * 3600e3) / 1000)),
  custom: (function () {
    const st = ctx.trangeState('t');
    st.preset = 'custom';
    st.from = new Date(2026, 8, 30, 9, 0, 0);
    st.to = new Date(2026, 8, 30, 18, 30, 0);
    return ctx.trangeQuery('t', true).toString();
  })(),
  labelCustom: ctx.trangeLabel('t'),
  labelToday: (function () { ctx.trangeState('t').preset = 'today'; return ctx.trangeLabel('t'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "trange-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("time range node test failed: %v\n%s", err, out)
	}
	local := func(h, m int) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, h, m, 0, 0, time.Local).Unix(), 10)
	}
	// rolling0 曾期望空串——那正是 issue #121：服务端「不给参数」默认 72 小时，空查询让
	// 「全部历史」退化成「近 3 天」。用量侧必须显式发 hours=0（归档侧仍空查询，见下条）。
	want := `{"todayIsMidnight":true,"todayNoTo":true,` +
		`"rolling24":"hours=24","rolling72":"hours=72","rolling0":"hours=0",` +
		`"log24From":true,"log24HasHours":false,"log7dFrom":true,` +
		`"custom":"from=` + local(9, 0) + `&to=` + local(18, 30) + `",` +
		`"labelCustom":"9-30 09:00 → 9-30 18:30","labelToday":"今天"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("time range=%s\nwant %s", out, want)
	}
}

// 同到期时间按面额降序；其余未用完包与零/负余额包分别聚合。
func TestAppJSDetailGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; detail groups test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_DEFAULT_DETAIL_LIMIT');
const end = src.indexOf('function renderPackages');
if (start < 0 || end < 0) throw new Error('detail group functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pkDetailGroups = pkDetailGroups; this.pkDetailLimit = pkDetailLimit;', ctx);
const input = [
  { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
  { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
  { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
  { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
  { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
  { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
  { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
  { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
];
const before = input.map(p => p.id).join(',');
const out = ctx.pkDetailGroups(input, 2);
process.stdout.write(JSON.stringify({
  visible: out.visible.map(p => p.id),
  rest: out.rest.map(p => p.id),
  used: out.used.map(p => p.id),
  restSize: out.restSize,
  restRemain: out.restRemain,
  usedSize: out.usedSize,
  defaultLimit: ctx.pkDetailLimit({}),
  configuredLimit: ctx.pkDetailLimit({ panel: { package_detail_limit: 7 } }),
  unchanged: input.map(p => p.id).join(',') === before,
}));`
	f, err := os.CreateTemp(t.TempDir(), "detail-groups-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("detail groups node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":["large-early","small-early"],"rest":["small-late","large-unknown","small-unknown"],"used":["zero-early-b","zero-early-a","zero-late"],"restSize":500,"restRemain":5,"usedSize":700,"defaultLimit":5,"configuredLimit":7,"unchanged":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("detail groups=%s want %s", out, want)
	}
}

// 精确剩余天数聚合、账号内按总余额钳制、无到期批次不进入图表。
func TestAppJSExpirySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry summary test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_ACCOUNT_COLORS');
const end = src.indexOf('function renderExpiryDistribution');
if (start < 0 || end < 0) throw new Error('expiry summary functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.summarizeCreditDays = summarizeCreditDays; this.pkAccountColorMap = pkAccountColorMap;', ctx);
const day = 86400000, now = 100000;
const out = ctx.summarizeCreditDays([
  { uid: 'a', remain: 100, packages: [
    { name: 'soon-a', remain: 30, expires_at: now + day },
    { name: 'later', remain: 70, expires_at: now + 7 * day },
  ] },
  { uid: 'b', remain: 55, packages: [
    { name: 'soon-b', remain: 20, expires_at: now + day },
    { name: 'unknown', remain: 5, end_time: '' },
  ] },
  { uid: 'err', error: 'offline' },
], now);
process.stdout.write(JSON.stringify({
  rows: out.rows.map(row => ({ days: row.days, credits: row.credits })),
  accountCount: out.accountCount,
  unavailable: out.unavailable,
  colorA: ctx.pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a'),
  colorB: ctx.pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry summary node test failed: %v\n%s", err, out)
	}
	const want = `{"rows":[{"days":1,"credits":50},{"days":7,"credits":70}],"accountCount":3,"unavailable":1,"colorA":"#4f8cff","colorB":"#25b08b"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry summary=%s want %s", out, want)
	}
}

func TestLogoutWired(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if !strings.Contains(rec.Body.String(), `id="btnLogout"`) {
		t.Error(`index.html 缺少登出按钮 id="btnLogout"`)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	if !strings.Contains(src, "$('btnLogout')") {
		t.Error("app.js 未绑定 $('btnLogout')：按钮点了不会有反应")
	}
	if !strings.Contains(src, "localStorage.removeItem(LS_KEY)") {
		t.Error("登出必须清掉本地密钥（LS_KEY），否则刷新后仍免密进入面板")
	}
}

// 积分扣除维度的格式必须稳定，且缺样本/缺匹配 Token 时不能伪造比例。

func TestQrowShowsCodeAndTitle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	harness := `const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
function extractFn(name) {
  const sig = 'function ' + name + '(';
  const start = src.indexOf(sig);
  if (start < 0) throw new Error('missing ' + name);
  let i = src.indexOf('{', start);
  let depth = 0, inStr = '';
  for (; i < src.length; i++) {
    const c = src[i];
    if (inStr) {
      if (c === '\\') { i++; continue; }
      if (c === inStr) inStr = '';
      continue;
    }
    if (c === '"' || c === "'" || c === String.fromCharCode(96)) { inStr = c; continue; }
    if (c === '{') depth++;
    else if (c === '}') { depth--; if (depth === 0) return src.slice(start, i + 1); }
  }
  throw new Error('unclosed ' + name);
}
const GROWTH_TITLES = {};
const ST_WORDS = { done: '完成', scan: '待执行' };
function esc(s) { return String(s == null ? '' : s); }
eval(extractFn('taskProgressLabel'));
eval(extractFn('qrowHTML'));
eval(extractFn('groupsFromQueue'));
const groups = groupsFromQueue([{ uid: 'u', nickname: 'n', kind: 'growth', code: 'Sequential_Tasks_4', title: '创建 1 个定时任务', status: 'done', message: 'm' }]);
const html = qrowHTML(groups[0].rows[0]);
const name = html.split('class="t">')[1].split('<')[0];
if (!html.includes('>Sequential_Tasks_4<') || name !== '创建 1 个定时任务') {
  console.log('FAIL', name, html);
  process.exit(1);
}
const bare = qrowHTML({ code: 'black_cat', status: 'done' });
const bareName = bare.split('class="t">')[1].split('<')[0];
if (bareName !== 'black_cat') {
  console.log('FAIL fallback', bareName);
  process.exit(1);
}
console.log('OK');
`
	hf, err := os.CreateTemp(t.TempDir(), "qrow-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	cmd := exec.Command(node, hf.Name(), "app.js")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("qrow title check: %v\n%s", err, out)
	}
}

// TestIndexHTMLConfigInputsUnique 表单里每个 name 只能出现一次。
//
// 为什么需要：配置表单是「按 name 取值」的（collectConfig 遍历 CFG_MAP → elements[name]），
// 同名输入框会静默互相覆盖（DOM 里靠后的赢），页面上看着像两个独立选项、实际只生效一个——
// 本仓库已经犯过两次（成长任务队列整行被重复插入）。这里把它挡在 CI。

func TestIndexHTMLConfigInputsUnique(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	re := regexp.MustCompile(`<input[^>]*>`)
	nameRe := regexp.MustCompile(`\sname="([^"]+)"`)
	typeRe := regexp.MustCompile(`\stype="([^"]+)"`)
	seen := map[string]int{}
	for _, tag := range re.FindAllString(body, -1) {
		nm, ty := nameRe.FindStringSubmatch(tag), typeRe.FindStringSubmatch(tag)
		// radio 组靠同名互斥，重复是正确写法；其余同名会互相覆盖。
		if nm == nil || (ty != nil && ty[1] == "radio") {
			continue
		}
		seen[nm[1]]++
	}
	if len(seen) == 0 {
		t.Fatal("no named inputs found in index.html")
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("input name=%q 出现 %d 次（同名会互相覆盖，只保留一个）", name, n)
		}
	}
}

// TestAppJSIdsExistInHTML 钉住「app.js 里 $('id') 引用的字面 id 必须在 index.html 里存在」。
// 为什么需要：元素缺失时 $('x').innerHTML = ... 抛 TypeError，而 loadLogs 这类函数把它吞在
// try/catch 里 —— 症状是整块视图静默空白（运行日志在合并 PR #87 时就丢过一次），
// 而所有 Go 测试与 JS 语法检查全绿。
func TestAppJSIdsExistInHTML(t *testing.T) {
	app, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range regexp.MustCompile(`\$\('([A-Za-z0-9_-]+)'\)`).FindAllStringSubmatch(string(app), -1) {
		n++
		if !strings.Contains(string(html), `id="`+m[1]+`"`) {
			t.Errorf("app.js 引用的 id 在 index.html 里不存在: %s", m[1])
		}
	}
	if n == 0 {
		t.Fatal("没有从 app.js 里扫到任何 id 引用，正则可能失效")
	}
}

// TestUsageChartSingleLegend 钉住「Token 时序」卡头只有一条图例、四个项目各出现一次。
//
// 为什么需要：合并上游 PR #87 时，排版重构版的图例（prompt/completion·均值）与 fork
// 的图例（prompt/completion/缓存命中率）两条被同时留下 —— 同一张图上并排两套重复条目，
// 用户读到「prompt completion 缓存命中率」和「prompt completion 均值」两种口径，无从
// 判断该信哪个。图例是纯静态 HTML：Go 测试与 JS 语法检查全绿，浏览器也不报错，回归
// 只能靠这条断言拦。
func TestUsageChartSingleLegend(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(html)
	start := strings.Index(body, `class="uschart-hd"`)
	if start < 0 {
		t.Fatal("index.html 里没有 uschart-hd 卡头")
	}
	end := strings.Index(body[start:], "</div>")
	if end < 0 {
		t.Fatal("uschart-hd 卡头没有闭合的 </div>")
	}
	hd := body[start : start+end]
	if n := strings.Count(hd, `class="legend"`); n != 1 {
		t.Errorf("Token 时序卡头应只有 1 条图例，实际 %d 条（重复图例=同一张图两种口径）", n)
	}
	for _, item := range []string{"prompt", "completion", "缓存命中率", "均值"} {
		if n := strings.Count(hd, item); n != 1 {
			t.Errorf("图例项 %q 在卡头出现 %d 次，应为 1 次", item, n)
		}
	}
}

// TestAppJSCollectConfigClearable 钉住 collectConfig 的空串语义（吸收上游 10e17ef）。
//
// 覆盖型字段（user_agent / prompt_file）空串必须照发：漏发会让面板显示"已保存"
// 而 config.json 里的值没变（issue #102 附带发现 2）。
//
// 同时钉住反面：其余文本字段空串仍然不下发。这条同样重要——若哪天为了修上面那个
// 问题改成"所有空串都发"，表单里任何一个没填的框都会变成"请清空"，静默抹掉配置。
func TestAppJSCollectConfigClearable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; collectConfig test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP');
const end = src.indexOf('/* Go 时长字段即时校验');
if (start < 0 || end < 0 || end < start) throw new Error('collectConfig region not found');
const mk = v => ({ type: 'text', value: v });
const cfgForm = { elements: {
  listen: mk(''),
  api_key: mk('secret'),
  user_agent: mk(''),
  prompt_file: mk(''),
  checkin_hours: mk(''),
}};
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams, Set,
  document: { getElementById: id => (id === 'cfgForm' ? cfgForm : null) },
  $: id => (id === 'cfgForm' ? cfgForm : null),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.collectConfig = collectConfig;', ctx);
const out = ctx.collectConfig();
const has = (o, k) => Object.prototype.hasOwnProperty.call(o || {}, k);
process.stdout.write(JSON.stringify([
  has(out.upstream, 'user_agent'), (out.upstream || {}).user_agent,
  has(out.prompt, 'file'), (out.prompt || {}).file,
  has(out, 'listen'),
  has(out.schedule, 'checkin_hours'),
  out.api_key
]));`
	f, err := os.CreateTemp(t.TempDir(), "cfgc-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("collectConfig node test failed: %v\n%s", err, out)
	}
	// [user_agent 已发, 其值, prompt.file 已发, 其值, listen 未发, checkin_hours 未发, api_key]
	const want = `[true,"",true,"",false,false,"secret"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("collectConfig=%s want %s", strings.TrimSpace(string(out)), want)
	}
}

// 图片模型的倍率格：上游不报倍率时用「实测 X/张」兜底（来源是本网关的实扣记录），
// 有牌价时仍旧显示牌价——牌价与实测必须分列，不能互相顶替。
func TestAppJSRateCellMeasuredFallback(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rateCell test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function rateCell(');
assert.ok(start >= 0, 'rateCell not found');
const ctx = { String, Object, esc: v => String(v == null ? '' : v) };
vm.createContext(ctx);
vm.runInContext(src.slice(start, src.indexOf('async function loadModels(')) + '\nthis.rateCell = rateCell;', ctx);
// ① 无牌价 + 有实测 → 实测 0.55/张
const a = ctx.rateCell({ credits: '', measured_credit: '0.55' });
assert.ok(a.includes('实测 0.55/张'), 'want 实测 0.55/张, got: ' + a);
// ② 有牌价 → 显示牌价，实测不顶替
const b = ctx.rateCell({ credits: 'x5.00', measured_credit: '5.71' });
assert.ok(b.includes('x5.00') && !b.includes('实测'), '牌价优先, got: ' + b);
// ③ 两者都无 → 仍是破折号（不编数字）
const c = ctx.rateCell({ credits: '' });
assert.equal(c, '—');
console.log('rateCell measured fallback passed');`
	path := filepath.Join(t.TempDir(), "ratecell.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestAppJSTrangeAllHistory 「全部历史」必须给用量接口发 hours=0。
//
// 为什么：服务端 /panel/api/usage 的契约是「不给参数 = 默认 72 小时、显式 0 = 全部历史」。
// 空查询因此让「全部历史」静默退化成「近 3 天」——线上实测同一时刻 全部历史 8903 次请求
// 与 近 3 天 完全一致，而真全部历史是 24817 次（issue #121 的形态）。
func TestAppJSTrangeAllHistory(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; trangeQuery test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function trangeQuery(');
assert.ok(start >= 0, 'trangeQuery not found');
const st = { preset: '0', from: null, to: null };
const ctx = { URLSearchParams, Date, Number, String, Object,
  trangeState: () => st, trangeMidnight: () => new Date('2026-10-06T00:00:00') };
vm.createContext(ctx);
vm.runInContext(src.slice(start, src.indexOf('function trangeLabel(')) + '\nthis.trangeQuery = trangeQuery;', ctx);
// ① 用量（rolling）：「全部历史」→ hours=0
st.preset = '0';
assert.equal(ctx.trangeQuery('usRange', true).toString(), 'hours=0', '全部历史应发 hours=0');
// ② 归档（非 rolling）：不认 hours，保持空查询
assert.equal(ctx.trangeQuery('reqRange', false).toString(), '');
// ③ 滚动预设照旧发 hours（回归：别把其它档也改了）
st.preset = '72';
assert.equal(ctx.trangeQuery('usRange', true).toString(), 'hours=72');
// ④ 今天 / 自定义走 from/to，不受影响
st.preset = 'today';
assert.ok(ctx.trangeQuery('usRange', true).get('from'));
st.preset = 'custom';
st.from = new Date('2026-10-01T00:00:00');
assert.ok(ctx.trangeQuery('usRange', true).get('from'));
console.log('trange all-history passed');`
	path := filepath.Join(t.TempDir(), "trange.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestConfigFormMatchesCFGMap 配置表单字段与 CFG_MAP 必须一一对应（吸收上游 7339a3c / PR #122）。
//
// 为什么需要：这套映射断掉时没有任何编译期或运行期报错——表单多一个字段，保存时被静默
// 丢弃；CFG_MAP 多一个键，回填/保存空转。两者都只能靠人点开配置页才发现。上游当时的
// 现场是 `logging.request_client_info` 在表单里被连带删掉，而 Go 侧配置键、热生效通路、
// README 描述都还在。
func TestConfigFormMatchesCFGMap(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)

	// CFG_MAP 块（下面两条检查共用）。
	mb := js[strings.Index(js, "const CFG_MAP = {"):]
	mb = mb[:strings.Index(mb, "\n};")]
	// 不能按行首匹配：CFG_MAP 里多个键写在同一行（`a: [...], b: [...]`），只有行首
	// 那个带换行缩进。按「前面是行首或分隔符」判定才不漏。
	inMap := func(name string) bool {
		return regexp.MustCompile(`(?:^|[\s,{])` + regexp.QuoteMeta(name) + `:\s*\[`).MatchString(mb)
	}

	// 1) 表单里的每个 name 都要有 CFG_MAP 条目（否则收集/回填都拿不到它）。
	f := html[strings.Index(html, `<form id="cfgForm">`):]
	f = f[:strings.Index(f, "</form>")]
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`name="([a-z_0-9]+)"`).FindAllStringSubmatch(f, -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("未从配置表单解析出任何 name 字段")
	}
	for n := range names {
		if !inMap(n) {
			t.Errorf("表单字段 %q 在 CFG_MAP 里没有条目（保存时会被静默丢弃）", n)
		}
	}

	// 2) CFG_MAP 里的每个键都要在表单里有控件（否则回填/保存是空转）。
	for _, m := range regexp.MustCompile(`(?:^|[\s,{])([a-z_0-9]+):\s*\[`).FindAllStringSubmatch(mb, -1) {
		if !names[m[1]] {
			t.Errorf("CFG_MAP 键 %q 在配置表单里没有对应控件", m[1])
		}
	}
}

// TestAppJSTaskCenterScanNotOverwritten 任务中心：切到「扫描/队列」结果后，5 秒定时
// 刷新不能把列表抢回后台任务视图。
//
// 为什么需要：refreshVisible()（每 5s）在 taskscenter 视图下调 reattachQueueView()，
// 而它无条件 taskCenterOwner='jobs' + loadTaskJobs() —— 用户刚点「扫描待办」查出来的
// 结果会在下一个 tick 被 renderQueue(空) 冲成空白，表现为「查完就自己消失，按好几次
// 才又有」。闸门只挡抢视图那一步，尾部「本页队列仍在跑就恢复轮询」不受影响。
func TestAppJSTaskCenterScanNotOverwritten(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; task center reattach test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('function reattachQueueView');
const e = src.indexOf('/* ── 用量');
assert.ok(s >= 0 && e > s, 'reattachQueueView not found');
const calls = [];
const ctx = {
  String, Number, Object, Array, Promise, JSON, console, setTimeout,
  api: async () => ({ started: false }),
  $: () => ({ hidden: false, textContent: '', style: {} }),
  loadTaskJobs: () => calls.push('jobs'),
  startQueuePolling: () => calls.push('polling'),
};
vm.createContext(ctx);
vm.runInContext('let queueTimer = null, lastQueueSeq = 0; let taskCenterOwner = "jobs";\n' +
  src.slice(s, e) +
  '\nthis.setOwner = v => { taskCenterOwner = v; }; this.owner = () => taskCenterOwner;' +
  '\nthis.reattach = reattachQueueView;', ctx);
(async () => {
  // ① 扫描结果占着列表：定时刷新不得抢视图、不得渲染后台任务
  ctx.setOwner('queue');
  ctx.reattach();
  await new Promise(r => setTimeout(r, 30));
  const held = { owner: ctx.owner(), calls: calls.slice() };
  // ② 明确回到后台任务视图（点「刷新进度」那条路）时照旧渲染
  ctx.setOwner('jobs');
  ctx.reattach();
  await new Promise(r => setTimeout(r, 30));
  process.stdout.write(JSON.stringify({ held: held, afterJobs: calls.slice() }));
})();`
	f, err := os.CreateTemp(t.TempDir(), "task-center-reattach-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("task center reattach node test failed: %v\n%s", err, out)
	}
	want := `{"held":{"owner":"queue","calls":[]},"afterJobs":["jobs"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("scan results not protected from periodic refresh: got=%s want=%s", out, want)
	}
}

// 积分构成「按需拉取」（移植上游 8391a05）：packages 是逐账号实时查上游，切进视图就打一次
// 会在账号多时既慢又压上游。断言四件事：冷缓存才打上游、热缓存直接渲染并标注数据年龄、
// 「刷新」按钮始终实时查（且不依赖事件对象真值）、失败不污染缓存（下次仍会重试）。
func TestAppJSPackagesCacheFirst(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; packages cache test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let lastPackages');
if (start < 0) throw new Error('packages cache not found in app.js');
let fakeNow = 1700000000000;
class FakeDate extends Date { static now() { return fakeNow; } }
const els = {}, calls = [], rendered = [];
const el = (id) => (els[id] = els[id] || { textContent: '', innerHTML: '' });
let failNext = false;
const ctx = {
  Date: FakeDate, Math, JSON, Promise, Number, String,
  $: el,
  api: async (p) => {
    calls.push(p);
    if (p === 'packages' && failNext) { failNext = false; throw new Error('boom'); }
    return p === 'packages' ? { accounts: [{ uid: 'u1' }], stamp: calls.length } : { config: {} };
  },
  renderPackages: (d, limit) => rendered.push([d.stamp, limit]),
  pkDetailLimit: () => 7,
  esc: (s) => String(s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start) + '\nthis.loadPackages = loadPackages;', ctx);
(async () => {
  await ctx.loadPackages();                       // 冷：首次进入 → 打上游
  const cold = { calls: calls.length, rendered: rendered.slice() };
  fakeNow += 3 * 60000;                           // 过 3 分钟
  el('pkNote').textContent = 'sentinel';
  await ctx.loadPackages();                       // 热：切回视图 → 不打上游，渲染缓存
  const warm = { calls: calls.length, rendered: rendered.slice(), note: el('pkNote').textContent };
  await ctx.loadPackages(true);                   // 显式强刷
  const forced = { calls: calls.length, rendered: rendered.slice() };
  el('pkNote').textContent = 'sentinel';
  await el('btnPk').onclick();                      // 按钮：无事件参数也必须强刷
  const viaBtn = { calls: calls.length, rendered: rendered.slice(), note: el('pkNote').textContent };
  failNext = true;                                // 上游失败：不得写进缓存
  await ctx.loadPackages(true);
  const failed = { calls: calls.length, rendered: rendered.length };
  await ctx.loadPackages();                       // 失败后仍吃旧缓存（即失败响应没被存进去）
  const retry = { calls: calls.length, last: rendered[rendered.length - 1] };
  process.stdout.write(JSON.stringify({
    cold: cold, warm: warm, forced: forced, viaBtn: viaBtn, failed: failed, retry: retry,
  }));
})();`
	f, err := os.CreateTemp(t.TempDir(), "packages-cache-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("packages cache node test failed: %v\n%s", err, out)
	}
	// calls 每轮 +2（packages + config）；渲染里的 stamp = 取自第几次调用，用来判「渲染的是哪一份数据」。
	// 末段：强刷失败后**再进入视图仍吃上一次成功的缓存**（stamp 仍是 5，不是失败那轮的 7）——
	// 失败响应若被写进缓存，这里就会渲染 7。
	want := `{"cold":{"calls":2,"rendered":[[1,7]]},` +
		`"warm":{"calls":2,"rendered":[[1,7],[1,7]],"note":` + strconv.Quote("1 个账号 · 3 分钟前的数据，点「刷新」更新") + `},` +
		`"forced":{"calls":4,"rendered":[[1,7],[1,7],[3,7]]},` +
		`"viaBtn":{"calls":6,"rendered":[[1,7],[1,7],[3,7],[5,7]],"note":"sentinel"},` +
		`"failed":{"calls":8,"rendered":4},` +
		`"retry":{"calls":8,"last":[5,7]}}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("packages 按需拉取行为不符:\n got=%s\nwant=%s", out, want)
	}
}

// 账号池页的到期提醒快照：只吃 /overview 已有的最早到期字段，不发任何上游请求。
// 钉三件事：按到期升序（最近的排最前）、无未来批次的账号不进表、剩余天数口径与
// pkExpiryText 一致（ceil 到天）——否则同一条批次在「积分构成」页与账号池页会各说一个数。
func TestAppJSExpirySnapshot(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry snapshot test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expCSTDate');
const end = src.indexOf('let lastPackages');
if (start < 0 || end < 0 || end < start) throw new Error('expiry snapshot region not found');
const els = {};
const ctx = {
  Date, Math, Number, String, isFinite,
  PK_DAY_MS: 86400000,
  $: id => (els[id] = els[id] || { innerHTML: '', textContent: '', addEventListener: () => {} }),
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  pkExpiryDateTime: ms => new Date(ms).toISOString(),   // 只取前 10 位，时区口径由真函数负责
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpirySnapshot = renderExpirySnapshot;', ctx);
const day = n => new Date(Date.now() + n * 86400000).toISOString();
ctx.renderExpirySnapshot({ accounts: [
  { uid: 'aaaaaaaa-1', nickname: '秋',     credits_earliest_expiry: day(20), credits_earliest_remaining: 499 },
  { uid: 'bbbbbbbb-2', nickname: '秋千易', credits_earliest_expiry: day(2),  credits_earliest_remaining: 41 },
  { uid: 'cccccccc-3', nickname: '长期包', credits_earliest_expiry: null,   credits_earliest_remaining: 0 },
  { uid: 'dddddddd-4', nickname: '零批次', credits_earliest_expiry: day(5), credits_earliest_remaining: 0 },
]});
const html = els['expBody'].innerHTML;
const cells = h => h.split('<tr>').slice(1).map(r =>
  [...r.matchAll(/<td[^>]*>([^<]*)<\/td>/g)].map(m => m[1]));
const rows = cells(html);
const note = els['expNote'].textContent;   // 两次渲染前先取，下同

// 5 个将到期账号 → 只展开最近 3 个，其余 2 个收纳成一行且默认收起。
ctx.renderExpirySnapshot({ accounts: [1,2,3,4,5].map(n => ({ uid: 'u' + n, nickname: 'A' + n,
  credits_earliest_expiry: day(n), credits_earliest_remaining: 10 * n })) });
const html2 = els['expBody'].innerHTML;
process.stdout.write(JSON.stringify({
  rows: rows.map(c => [c[0], c[2], c[3], c[4]]),
  note: note,
  head: cells(html2.split('<tr class="pk-group-summary"')[0]).map(c => [c[0], c[2]]),
  restHidden: (html2.match(/<tr data-exp-row="rest" hidden>/g) || []).length,
  label: (html2.match(/data-exp-rest="\d+"[^>]*>([^<]+)</) || ['', ''])[1],
}));`
	f, err := os.CreateTemp(t.TempDir(), "expsnap-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry snapshot node test failed: %v\n%s", err, out)
	}
	// 41 分 2 天后到期 → 日均需耗 21（ceil 20.5）；499 分 20 天 → 25（ceil 24.95）。
	// 若把 ceil 改成 floor，这里必须变成 20 / 24。
	want := `{"rows":[["秋千易","2 天","41","21"],["秋","20 天","499","25"]],` +
		`"note":` + strconv.Quote("2 个账号有未到期批次 · 未开启后台刷新 · 明细见「积分构成」") + `,` +
		// 收纳：只展开最近到期的 3 个（A1/A2/A3 按到期升序），其余 2 个 hidden。
		`"head":[["A1","明天"],["A2","2 天"],["A3","3 天"]],"restHidden":2,"label":"其余 2 个账号，展开"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("到期快照渲染不符:\n got=%s\nwant=%s", out, want)
	}
}

// 账号备注：键取 uid 前 8 位（与 renderUsageDim 账号维度、积分扣除表的 uid 前缀同源，
// 三个表格因此能直接 join）；有备注时备注当主标识、原昵称作浅色虚体跟随；备注来自本机
// 输入，必须转义后才能拼进 innerHTML。
func TestAppJSRemarkName(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; remark name test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const start = src.indexOf('function remarkKey(');
const end = src.indexOf('function storeRemarks(');
if (escStart < 0 || escEnd < 0) throw new Error('esc helpers not found');
if (start < 0 || end < 0) throw new Error('remark helpers not found');
const ctx = { String, RegExp, Number };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(start, end) +
  '\nconst REM = {abcdef12: "JTOL", deadbeef: "<b>x</b>"};' +
  '\nfunction remarksAll() { return REM; }' +
  '\nthis.remarkKey=remarkKey; this.remarkName=remarkName;',
  ctx
);
process.stdout.write(JSON.stringify({
  key: ctx.remarkKey('ABCDEF1234567890'),
  short: ctx.remarkKey('abc'),
  empty: ctx.remarkKey(null),
  remarked: ctx.remarkName('abcdef12', '张三'),
  plain: ctx.remarkName('zzzzzzzz', '张三'),
  escaped: ctx.remarkName('deadbeef', 'n'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "remark-name-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("remark name node test failed: %v\n%s", err, out)
	}
	const want = `{"key":"ABCDEF12","short":"abc","empty":"","remarked":"JTOL<span class=\"rm-ghost\">张三</span>","plain":"张三","escaped":"&lt;b&gt;x&lt;/b&gt;<span class=\"rm-ghost\">n</span>"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("remark name=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// 备注与折叠的接线齐备性：少一个 id 或漏一个 localStorage 键，表现为「按钮点了没反应」，
// 而 Go 编译、JS 语法检查、顶层冒烟都发现不了（脚本能正常跑，只是挂不上）。
// 本仓只取了 PR #178 的备注部分（积分到期折叠未移植：到期提醒已重做为图表式），
// 故这里只查备注相关 id / 键 / 函数。
func TestPanelRemarkWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="btnRemarks"`, `id="remarkVeil"`, `id="remarkBody"`,
		`id="btnRemarkSave"`, `id="btnRemarkClear"`, `id="btnRemarkClose"`,
	} {
		if !bytes.Contains(html, []byte(want)) {
			t.Fatalf("index.html 缺少 %s", want)
		}
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"LS_REMARKS = 'wb2api.remarks.v1'",
		"function remarkKey(",
		"function remarkName(",
		"function storeRemarks(",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js 缺少 %s", want)
		}
	}
}

// TestAppJSCompensationCard 官方补偿卡片（上游 PR #196）：待领显形 + 跳过带原因 +
// 失败红行 + ≤48h 标红 + 领取按钮只出现在待领行。
//
// 为什么值得一测：这张卡的全部价值就是「看不见的状态变成看得见」——渲染层若把
// 跳过/失败行吞掉，就把「查不到」显示成「没有补偿」，正是本改动要消灭的界面谎言。
func TestAppJSCompensationCard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; compensation card test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* ══ 官方补偿（上游 PR #196）');
const end = src.indexOf('let lastPackages');
if (start < 0 || end < start) throw new Error('compensation block not found');
// 收纳行的点击处理在到期提醒那段（两个卡片共用），单独切过来一起跑。
const tStart = src.indexOf('function bindRestToggle(id)');
const tEnd = src.indexOf("bindRestToggle('compBody');") + "bindRestToggle('compBody');".length;
if (tStart < 0 || tEnd < tStart) throw new Error('bindRestToggle not found');
const toggleSrc = src.slice(tStart, tEnd);
const els = {};
function el(id) {
  if (!els[id]) els[id] = {
    textContent: '', innerHTML: '', hidden: false, rows: [], handlers: {},
    addEventListener(ev, fn) { this.handlers[ev] = fn; },
    querySelectorAll() { return this.rows; },
  };
  return els[id];
}
const now = Date.parse('2026-10-20T00:00:00+08:00');
class FakeDate extends Date { static now() { return now; } }
const ctx = {
  Date: FakeDate, Math, Number, String, JSON, encodeURIComponent,
  PK_DAY_MS: 86400000,
  $: el,
  esc: (s) => String(s == null ? '' : s),
  fmtTok: (n) => String(n),
  toast: () => {},
  api: async () => ({}),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\n' + toggleSrc +
  '\nthis.renderCompensation = renderCompensation; this.setData = (d) => { compData = d; };', ctx);

ctx.setData({ errored: 1, accounts: [
  { uid: 'bbbbbbbb-2', nickname: '已领号', active: true, claimed: true, credit: 500, end_time: '2026-10-22 23:59:59' },
  { uid: 'cccccccc-3', nickname: '跳过号', skip: true, skip_reason: '国际区账号不适用国内区补偿' },
  { uid: 'dddddddd-4', nickname: '失败号', error: 'boom' },
  { uid: 'eeeeeeee-5', nickname: '无活动号' },
  { uid: 'aaaaaaaa-1', nickname: '待领号', active: true, claimed: false, credit: 500, end_time: '2026-10-21 23:59:59' },
] });
ctx.renderCompensation();
const body = els['compBody'].innerHTML;
const note = els['compNote'].innerHTML;
process.stdout.write(JSON.stringify({
  hasPending: body.includes('待领'),
  pendingBtn: body.includes('data-comp="aaaaaaaa-1"'),
  claimedNoBtn: !body.includes('data-comp="bbbbbbbb-2"'),
  skipLine: body.includes('跳过') && body.includes('国际区账号不适用国内区补偿'),
  failLine: body.includes('查询/领取失败'),
  noActivity: body.includes('无活动'),
  soonRed: body.includes('var(--bad)'),
  notePending: note.includes('待领 1 个'),
  noteErrored: note.includes('1 个查询失败'),
  // 收纳：待领排最前、只展开前 3 行、其余 2 行收在 hidden 里、收纳按钮带计数
  pendingFirst: body.indexOf('待领号') < body.indexOf('已领号'),
  hiddenRows: (body.match(/data-exp-row="rest" hidden/g) || []).length,
  toggle: body.includes('data-exp-rest="2"') && body.includes('其余 2 个账号，展开'),
  // 点一次收纳按钮：2 行必须真展开（展开逻辑与到期提醒共用 bindRestToggle）
  expandWorks: (() => {
    const rows = [{ hidden: true }, { hidden: true }];
    const tbody = els['compBody'];
    tbody.rows = rows;
    const btn = {
      dataset: { expRest: '2' }, textContent: '', attrs: {},
      getAttribute(k) { return this.attrs[k]; },
      setAttribute(k, v) { this.attrs[k] = v; },
      closest() { return tbody; },
    };
    const click = () => tbody.handlers.click({
      target: { closest: (sel) => (sel === 'button[data-exp-rest]' ? btn : null) },
    });
    click();
    const opened = rows.every(r => r.hidden === false) && btn.textContent === '收起其余 2 个账号';
    click();
    return opened && rows.every(r => r.hidden === true) && btn.textContent === '其余 2 个账号，展开';
  })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "comp-card-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("compensation card node test failed: %v\n%s", err, out)
	}
	const want = `{"hasPending":true,"pendingBtn":true,"claimedNoBtn":true,"skipLine":true,` +
		`"failLine":true,"noActivity":true,"soonRed":true,"notePending":true,"noteErrored":true,` +
		`"pendingFirst":true,"hiddenRows":2,"toggle":true,"expandWorks":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("补偿卡片渲染不符:\n got=%s\nwant=%s", strings.TrimSpace(string(out)), want)
	}
}

// TestPanelCompensationWiring 接线齐备性：少一个 id 或漏一个绑定，表现为「按钮点了没反应」，
// 而 Go 编译、JS 语法检查、顶层冒烟都发现不了。
func TestPanelCompensationWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="compNote"`, `id="compBody"`, `id="btnCompRefresh"`, `id="btnCompAll"`,
	} {
		if !bytes.Contains(html, []byte(want)) {
			t.Fatalf("index.html 缺少 %s", want)
		}
	}
	// 卡片必须落在「任务中心」视图内（它讲的是「今天要不要动手」，与成长任务同处）——
	// 只查 id 存在的话，放回账号池视图也照样通过。
	tc := bytes.Index(html, []byte(`id="view-taskscenter"`))
	next := bytes.Index(html[tc+1:], []byte(`<section class="view"`))
	if tc < 0 || next < 0 {
		t.Fatal("定位不到任务中心视图边界")
	}
	for _, id := range []string{`id="compNote"`, `id="compBody"`, `id="btnCompAll"`} {
		at := bytes.Index(html, []byte(id))
		if at < tc || at > tc+1+next {
			t.Fatalf("%s 不在任务中心视图内（视图区间 %d..%d，实际 %d）", id, tc, tc+1+next, at)
		}
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"function renderCompensation(",
		"function loadCompensation(",
		"function claimCompensation(",
		"function claimCompensationAll(",
		"$('btnCompRefresh').onclick",
		"$('btnCompAll').onclick",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js 缺少 %s", want)
		}
	}
	// 取数时机随卡片一起搬：进任务中心按需拉（compData 空才打上游），start() 不再拉。
	if !bytes.Contains(js, []byte("if (v === 'taskscenter') { reattachQueueView(); if (!compData) loadCompensation(); }")) {
		t.Error("app.js 未在进入任务中心时按需拉取补偿状态")
	}
	start := bytes.Index(js, []byte("function start() {"))
	if start < 0 {
		t.Fatal("找不到 start()")
	}
	if bytes.Contains(js[start:start+200], []byte("loadCompensation()")) {
		t.Error("start() 仍在首屏拉补偿状态（已随卡片搬走）")
	}
}

// TestAppJSModelBusyTag 模型行渲染行为：busy/congested 必须出标签（bad 色），
// idle/normal 与缺字段必须安静——「模型与档位」是唯一能看到 congested/unavailable 的地方。
// 行为断言而非字符串断言：后者在 `if (false)` 这种「代码在但永远不跑」下仍会通过。
func TestAppJSModelBusyTag(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model busy tag test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('// 上游模型繁忙度（/v2/chat/busy）文案');
const end = src.indexOf('function renderModels()');
if (start < 0 || end < start) throw new Error('busy block not found');
const ctx = {
  esc: (s) => String(s == null ? '' : s),
  rateCell: () => '<td></td>',
  outCell: () => '<td></td>',
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rowHtml = mdRowHtml;', ctx);
const base = { id: 'cn:glm-5.2', supported_efforts: [], image_generation: false };
const out = {
  congested: ctx.rowHtml({ ...base, busy: 'congested' }, null),
  busy: ctx.rowHtml({ ...base, busy: 'busy' }, null),
  idle: ctx.rowHtml({ ...base, busy: 'idle' }, null),
  normal: ctx.rowHtml({ ...base, busy: 'normal' }, null),
  none: ctx.rowHtml(base, null),
};
process.stdout.write(JSON.stringify({
  congestedBad: out.congested.includes('拥堵') && out.congested.includes('tag bad'),
  busyWarn: out.busy.includes('繁忙') && out.busy.includes('tag warn'),
  idleQuiet: !out.idle.includes('拥堵') && !out.idle.includes('繁忙'),
  normalQuiet: !out.normal.includes('拥堵') && !out.normal.includes('繁忙'),
  noneQuiet: !out.none.includes('拥堵') && !out.none.includes('繁忙'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "busy-tag-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model busy tag node test failed: %v\n%s", err, out)
	}
	const want = `{"congestedBad":true,"busyWarn":true,"idleQuiet":true,"normalQuiet":true,"noneQuiet":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("繁忙度标签渲染不符:\n got=%s\nwant=%s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySnapshotBuckets 到期提醒快照必须消费 credits_expiry_buckets：
// 同日多包取聚合后的该日合计、后续批次列在日期下方、7 天内合计可读、排序稳定，
// 并在旧 state 无 buckets 时退回最早一批兜底。节奏标注必须来自 overview 的
// balance_refresh_sec/balance_refreshed_at，而不是界面轮询间隔。
func TestAppJSExpirySnapshotBuckets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry snapshot test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expCSTDate');
const end = src.indexOf('// 收纳行展开/收起');
if (start < 0 || end < 0) throw new Error('expiry snapshot helpers not found');
const ctx = {
  Number, String, Array, Date, Math, JSON, PK_DAY_MS: 86400000,
  expBody: { innerHTML: '' }, expNote: { textContent: '' },
  esc: s => String(s), fmtTok: n => String(n),
  pkExpiryDateTime: ms => new Date(ms).toISOString(),
};
ctx.$ = id => ctx[id];
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpirySnapshot=renderExpirySnapshot;', ctx);
const day = n => {
  const d = new Date(Date.now() + n * 86400000);
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(d);
  const p = t => (parts.find(x => x.type === t) || {}).value;
  return p('year') + '-' + p('month') + '-' + p('day');
};
ctx.renderExpirySnapshot({
  balance_refresh_sec: 300,
  balance_refreshed_at: new Date().toISOString(),
  accounts: [
    { uid: 'b', nickname: 'B', credits_expiry_buckets: [
      { date: day(5), remain: 300 }, { date: day(3), remain: 120 }] },
    { uid: 'a', nickname: 'A', credits_expiry_buckets: [{ date: day(3), remain: 150 }] },
  ],
});
const buckets = { html: ctx.expBody.innerHTML, note: ctx.expNote.textContent };
ctx.renderExpirySnapshot({
  accounts: [{ uid: 'c', nickname: 'C',
    credits_earliest_expiry: new Date(Date.now() + 3 * 86400000).toISOString(),
    credits_earliest_remaining: 77 }],
});
process.stdout.write(JSON.stringify({
  order: buckets.html.indexOf('>A<') >= 0 && buckets.html.indexOf('>A<') < buckets.html.indexOf('>B<'),
  hasRest: buckets.html.includes('随后'),
  weekSum: buckets.html.includes('7 天内合计 420'),
  dayLabel: buckets.html.includes('3 天'),
  note: buckets.note,
  fallback: ctx.expBody.innerHTML.includes('77'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-snapshot-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry snapshot node test failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	for _, want := range []string{
		`"order":true`, `"hasRest":true`, `"weekSum":true`, `"dayLabel":true`, `"fallback":true`,
		`每 5 分钟后台刷新`, `2 个账号有未到期批次`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expiry snapshot 输出缺少 %q: %s", want, got)
		}
	}
}
