package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSSetHTMLSkipsUnchanged 钉住 setHTML 的「内容相同就不碰 DOM」语义
// （perf：面板每 5 秒重建整张表，即使内容没变也在白烧 CPU/内存）。
//
// 断言的是**行为**而非实现：同一个元素连续两次写相同内容 → 第二次不得触发
// innerHTML 赋值；写不同内容 → 必须赋（否则页面会停在旧数据上）。
func TestAppJSSetHTMLSkipsUnchanged(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; setHTML test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 取出 setHTML 及其依赖（lastHTML 声明）那一段。
const start = src.indexOf('let lastHTML = new WeakMap();');
const end = src.indexOf('function ago(');
if (start < 0 || end < 0) throw new Error('setHTML block not found');
let assigns = 0;
const el = { set innerHTML(v) { assigns++; this._h = v; }, get innerHTML() { return this._h; } };
const ctx = { WeakMap, Map };
vm.createContext(ctx);
vm.runInContext('\'use strict\';' + src.slice(start, end) + '\nthis.setHTML=setHTML;', ctx);
ctx.setHTML(el, '<b>x</b>');
const afterFirst = assigns;
ctx.setHTML(el, '<b>x</b>');
const afterSame = assigns;
ctx.setHTML(el, '<b>y</b>');
const afterDiff = assigns;
process.stdout.write(JSON.stringify({ afterFirst, afterSame, afterDiff }));
`
	f, err := os.CreateTemp(t.TempDir(), "sethtml-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), appJSPath(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("setHTML node test failed: %v\n%s", err, out)
	}
	s := strings.TrimSpace(string(out))
	if s != `{"afterFirst":1,"afterSame":1,"afterDiff":2}` {
		t.Fatalf("setHTML 行为不对: %s（首次赋值=1、相同内容不再赋值=1、不同内容再赋值=2）", s)
	}
}

// TestAppJSLogIncrementalWiring 钉住运行日志的增量拉取接线：前端必须带 since
// 参数（否则每次都拉全量 500 行，~90KB），且服务端支持该参数。
//
// 这条是「省掉 logs 页 130× 流量」的接线闸门——哪天有人把 since 参数删了、
// 或增量合并逻辑去掉，这条测试会失败。
func TestAppJSLogIncrementalWiring(t *testing.T) {
	raw, err := os.ReadFile(appJSPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	// 增量请求：真正拼进 URL 的那行（不能只匹配注释里的字样——注释里也写了
	// "since_seq="，删掉代码它照样能匹配上，这条断言就废了）。
	if !strings.Contains(s, "const lq = logSeq ? '?since_seq='") {
		t.Error("运行日志未使用 ?since_seq= 增量参数（每轮会重拉全量 500 行）")
	}
	// 增量合并：把新行追加到本地窗口
	if !strings.Contains(s, "logRows = logRows.concat(incoming)") {
		t.Error("运行日志增量行未并入本地窗口（logRows.concat）")
	}
	// 游标推进
	if !strings.Contains(s, "logSeq") {
		t.Error("运行日志缺增量游标（logSeq）")
	}
	// 游标过期回落全量：长时间切走后服务端环形缓冲会淘汰中间行
	if !strings.Contains(s, "LOG_CURSOR_MAX_AGE_MS") {
		t.Error("运行日志缺游标过期回落（LOG_CURSOR_MAX_AGE_MS）")
	}
}

// TestAppJSLogCursorUsesSeq 钉住「游标是服务端单调序号 seq，不是时间戳」。
//
// 回归位（用户实测）：游标曾按 ts 推进。入环 TS 来自 time.Now()，同一时钟刻度
// 内的多次写入**完全相同**（Windows 实测 ~15.6ms 粒度），于是「最后一条」自己
// 仍然满足 `TS > since`，每轮轮询都被重发、游标再也不前进——「运行日志」底部
// 那行反复刷出来。曾经用 Date.toISOString() 取游标，毫秒截断让重复更严重。
//
// 断言的是**实际赋值的代码形态**，不是注释里的字样。
func TestAppJSLogCursorUsesSeq(t *testing.T) {
	raw, err := os.ReadFile(appJSPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "e.seq > logSeq) logSeq = e.seq") {
		t.Error("运行日志游标未按服务端 seq 推进（按 ts 推进会在同一时钟刻度内卡死）")
	}
	if strings.Contains(s, "logSinceTs") {
		t.Error("运行日志又退回 ts 游标（logSinceTs）：同一刻度内多条时间戳相同，必然重复或漏行")
	}
}

// TestAppJSPollPausedWhenHidden 钉住「页面隐藏时暂停轮询」——切到后台的标签页
// 仍在每 5 秒打接口 + 重建整表，是「面板很占资源」的一大来源。
func TestAppJSPollPausedWhenHidden(t *testing.T) {
	raw, err := os.ReadFile(appJSPath(t))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "visibilitychange") {
		t.Error("面板未监听 visibilitychange（页面隐藏后仍持续轮询）")
	}
	if !strings.Contains(s, "pageHidden") {
		t.Error("面板缺 pageHidden 状态（无法按可见性停表）")
	}
	if !strings.Contains(s, "if (pageHidden) return;") {
		t.Error("refreshVisible 未按 pageHidden 早退")
	}
}

// appJSPath 返回 app.js 的路径（测试工作目录是 internal/panel）。
func appJSPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("app.js 不存在: %v", err)
	}
	return p
}
