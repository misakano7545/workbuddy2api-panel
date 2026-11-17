package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 实时任务卡的前端测试夹具：用真实的 app.js 片段（qrowHTML、实时任务块、
// reattachQueueView、go）在 node 沙箱里跑。各用例只写 body，夹具提供 liveReply /
// apiFail / timers / timeouts / calls / nodes / doc / ctx / flush / liveCalls。
const liveNodePrelude = `
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const slice = (from, to) => { const a = src.indexOf(from), b = src.indexOf(to, a); assert(a > 0 && b > a, from); return src.slice(a, b); };
const nodes = new Map(), timers = new Map(), calls = [];
let tick = 0, liveReply = {accounts: []}, innerWrites = 0, apiFail = false;
const timeouts = new Map();
const node_ = id => {
  const n = {textContent: '', hidden: false, disabled: false, style: {}, classList: {add(){}, remove(){}}, addEventListener(){},
    querySelector: () => ({textContent: ''}), value: ''};
  let html = '';
  Object.defineProperty(n, 'innerHTML', {get: () => html, set: v => { html = v; if (id === 'liveList') innerWrites++; }});
  return n;
};
const doc = {hidden: false, addEventListener(){}, querySelectorAll: () => []};
const ctx = {
  console, Promise, Object, Array, String, Number, Map, JSON, encodeURIComponent, document: doc,
  $: id => { if (!nodes.has(id)) nodes.set(id, node_(id)); return nodes.get(id); },
  api: path => { calls.push(path); return apiFail ? Promise.reject(new Error('boom')) : Promise.resolve(path.startsWith('tasks/live') ? liveReply : {}); },
  esc: s => String(s == null ? '' : s).replace(/</g, '&lt;'),
  GROWTH_TITLES: {}, ST_WORDS: {done: '已完成', claim_pending: '待领取', error: '出错'}, taskProgressLabel: s => s || '',
  view: 'accounts', TITLES: {taskscenter: 't'}, navSet(){}, accCloseLayer(){}, renderAccBulk(){}, resetLogWindow(){}, logRows: [],
  loadModels(){}, loadModelRules(){}, loadConfig(){}, loadProxies(){}, loadEvents(){}, loadLogs(){}, loadUsage(){}, loadKeys(){},
  loadDiagnostics(){}, loadSched(){}, loadPackages(){}, loadBalance(){}, openTasks(){},
  taskCenterOwner: 'jobs', loadTaskJobs(){}, queueTimer: null, lastQueueSeq: 0, startQueuePolling(){},
  setInterval: (fn, ms) => { const id = ++tick; timers.set(id, {fn, ms}); return id; },
  clearInterval: id => timers.delete(id), setTimeout: (fn, ms) => { const id = ++tick; timeouts.set(id, {fn, ms}); return id; }, clearTimeout: id => timeouts.delete(id),
};
vm.createContext(ctx);
vm.runInContext(slice('function qrowHTML(', '/* 任务中心筛选'), ctx);
vm.runInContext(slice('/* ── 实时任务', 'async function loadTasks('), ctx);
vm.runInContext(slice('function reattachQueueView()', '/* ── 用量'), ctx);
vm.runInContext(slice('function go(v) {', "document.querySelectorAll('.nav a').forEach(a => a.onclick"), ctx);
const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
const liveCalls = () => calls.filter(p => p.startsWith('tasks/live')).length;
`

// runLiveNode 在夹具后接上用例脚本并执行；没有 node 时跳过（与其它前端测试一致）。
func runLiveNode(t *testing.T, name, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := liveNodePrelude + "\n(async () => {\n" + body + "\n})().catch(err => { console.error(err); process.exitCode = 1; });"
	path := filepath.Join(t.TempDir(), name+".cjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node test failed: %v\n%s", err, out)
	}
}
