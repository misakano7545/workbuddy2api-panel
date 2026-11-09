package panel

import (
	"os"
	"strings"
	"testing"
)

// 导入是两步动作：选中文件先出预览（dryRun），点「确认导入」才写盘。
//
// 这条契约靠三个前端事实保证，缺一个就会退化成「一点就导入」：
//  1. 文件选择框是多选的，且预览区与确认按钮都在页面上；
//  2. onchange 发的是 dryRun 预览，不是写入；
//  3. 写入路径上有 importPending.ready 这道闸——没成功预览过就不让写。
//
// 变异判据：把 ready 判断删掉、或把 onchange 里的 postImport(true) 改成 false，
// 本用例都会红。
func TestAppJSImportPreviewsBeforeCommit(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{`id="importFile"`, `id="importPreview"`, `id="btnImportCommit"`} {
		if !strings.Contains(h, want) {
			t.Fatalf("index.html 缺少 %s（预览区/确认按钮是多文件导入的组成部分）", want)
		}
	}
	// 文件框必须带 multiple，否则「多文件批量导入」名存实亡。
	idx := strings.Index(h, `id="importFile"`)
	if idx < 0 {
		t.Fatal("index.html 里找不到 importFile")
	}
	tail := h[idx:]
	end := strings.Index(tail, ">")
	if end < 0 || !strings.Contains(tail[:end], "multiple") {
		t.Fatalf("importFile 必须带 multiple：%s", tail[:min(end+1, len(tail))])
	}

	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	for _, want := range []string{
		"const d = await postImport(true);",  // 选择文件 → 预览
		"const d = await postImport(false);", // 确认 → 写入
		"if (!importPending || !importPending.ready) return;",
		"fd.append('dryRun', 'true');",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺少导入两步契约的关键片段：%q", want)
		}
	}

	// onchange 里不得出现写入调用：否则选完文件就直接落盘了。
	start := strings.Index(js, "$('importFile').onchange")
	commit := strings.Index(js, "$('btnImportCommit').onclick")
	if start < 0 || commit < 0 || commit < start {
		t.Fatal("app.js 找不到 importFile.onchange / btnImportCommit.onclick 的绑定顺序")
	}
	if body := js[start:commit]; strings.Contains(body, "postImport(false)") {
		t.Fatal("选择文件时不得直接写入：写入必须发生在「确认导入」之后")
	}
}
