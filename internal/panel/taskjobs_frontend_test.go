package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the actual click/poll lifecycle: a 202 response must keep showing
// background progress, closing must stop only polling, and a stale response must
// never replace the progress for another account.
func TestAppJSBackgroundTaskLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let taskUID = null'), end = src.indexOf('async function loadTasks(');
assert(start > 0 && end > start);
const nodes = new Map(), timers = new Map(), calls = [], toasts = [];
let tick = 0, reads = () => Promise.resolve({job: null});
const job = (uid, status, results = []) => ({id: 'job-' + uid, uid, status, trigger: 'manual', completed: 3, total: 25, current_task: 'chat_5', results});
const ctx = {
  console, Promise, Object, Array, String, Number, Map, encodeURIComponent,
  $: id => { if (!nodes.has(id)) nodes.set(id, {textContent: '', innerHTML: '', hidden: false, disabled: false, classList: {add(){}, remove(){}}, addEventListener(){}}); return nodes.get(id); },
  api: (path, opts) => { calls.push({path, method: opts?.method || 'GET'}); return opts?.method === 'POST' ? Promise.resolve({ok:true, started:true, job:job('cn-a','pending')}) : reads(path); },
  toast: (...x) => toasts.push(x),
  esc: s => String(s || '').replace(/</g, '&lt;'),
  qrowHTML: x => JSON.stringify(x),
  ago: () => '刚刚', view: 'accounts', loadTasks(){},
  setInterval: fn => { const id=++tick; timers.set(id, fn); return id; },
  clearInterval: id => timers.delete(id)
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
const flush = async () => { for(let i=0;i<10;i++) await Promise.resolve(); };
(async () => {
  vm.runInContext("taskUID='cn-a'; taskCapabilities={automate:true};", ctx);
  await nodes.get('btnTaskAutoAll').onclick();
  assert.strictEqual(nodes.get('btnTaskAutoAll').disabled, true);
  assert(nodes.get('taskJobState').textContent.includes('排队中'));
  assert.strictEqual(timers.size, 1);
  vm.runInContext('closeTasks()', ctx);
  assert.strictEqual(timers.size, 0);
  assert.strictEqual(calls.filter(c => c.method === 'POST').length, 1);

  reads = () => Promise.resolve({job: job('cn-a', 'running')});
  vm.runInContext("openTasks('cn-a')", ctx); await flush();
  assert(nodes.get('taskJobState').textContent.includes('执行中'));
  // 两个定时器：作业进度轮询 + 任务表静默刷新（30s，弹窗开着就一直跑）
  assert.strictEqual(timers.size, 2);
  reads = () => Promise.resolve({job: job('cn-a','finished', [{status:'done',claimed:true},{status:'awaiting_progress',message:'未计分'}])});
  // 弹窗里现在有两个定时器（作业轮询 + 任务静默刷新），创建顺序取决于异步竞态，
  // 不按下标取，全部推一拍——各自只更新自己那部分界面。
  for (const fn of [...timers.values()]) await fn();
  await flush();
  assert(nodes.get('taskJobState').textContent.includes('等待计分 1 项'));
  assert(nodes.get('taskJobState').textContent.includes('已结束'));
  assert.strictEqual(nodes.get('btnTaskAutoAll').disabled, false);
  // 作业跑完后作业轮询停了，但弹窗还开着，任务静默刷新仍在（只剩 1 个）
  assert.strictEqual(timers.size, 1);

  let releaseOld;
  reads = path => path.includes('cn-a') ? new Promise(resolve => releaseOld=resolve) : Promise.resolve({job:job('cn-b','running')});
  vm.runInContext("openTasks('cn-a'); openTasks('cn-b')", ctx); await flush();
  releaseOld({job:job('cn-a','finished')}); await flush();
  assert.strictEqual(vm.runInContext('taskJobID', ctx), 'job-cn-b');
  assert(nodes.get('taskJobState').textContent.includes('执行中'));
  vm.runInContext('closeTasks()', ctx);
  assert.strictEqual(timers.size, 0);
  console.log('Background task lifecycle OK');
})().catch(err => { console.error(err); process.exitCode=1; });`
	path := filepath.Join(t.TempDir(), "taskjobs-lifecycle.cjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("background task UI: %v\n%s", err, out)
	}
}
