package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 单号任务弹窗的 30s 静默刷新：标签页在后台时不发请求。
func TestAppJSTaskDialogRefreshSkipsHiddenTab(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let taskUID = null'), end = src.indexOf('async function loadTasks(');
assert(start > 0 && end > start);
const nodes = new Map(), timers = [], loads = [];
const doc = {hidden: false};
const ctx = {
  console, Promise, Object, Array, String, Number, Map, encodeURIComponent, document: doc,
  $: id => { if (!nodes.has(id)) nodes.set(id, {textContent: '', innerHTML: '', hidden: false, classList: {add(){}, remove(){}}, addEventListener(){}}); return nodes.get(id); },
  api: () => Promise.resolve({job: null}), toast(){}, esc: String, qrowHTML: () => '', ago: () => '', view: 'accounts',
  loadTasks: silent => loads.push(!!silent),
  setInterval: fn => { timers.push(fn); return timers.length; }, clearInterval(){},
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end), ctx);
(async () => {
  vm.runInContext("openTasks('cn-a')", ctx);
  const silentTimer = timers[timers.length - 1];
  loads.length = 0;
  silentTimer(); assert.deepStrictEqual(loads, [true], '前台应静默刷新');
  doc.hidden = true; loads.length = 0;
  silentTimer(); assert.deepStrictEqual(loads, [], '后台标签页不该刷新');
  console.log('dialog refresh OK');
})().catch(err => { console.error(err); process.exitCode = 1; });`
	path := filepath.Join(t.TempDir(), "taskdialog-hidden.cjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node test failed: %v\n%s", err, out)
	}
}
