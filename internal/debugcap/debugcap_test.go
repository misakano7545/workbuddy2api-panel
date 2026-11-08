package debugcap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCaptureDisabledByDefault 默认（未设环境变量）必须完全不写盘。
func TestCaptureDisabledByDefault(t *testing.T) {
	t.Setenv("WB2API_CAPTURE_DIR", "")
	Reset()
	if Enabled() {
		t.Fatal("未设目录时不应启用")
	}
	Capture("x", map[string]any{"a": 1})
	if Wrote() != 0 {
		t.Fatalf("禁用时不应写盘，写了 %d 行", Wrote())
	}
}

// TestCaptureWritesJSONL 启用后按 tag 追加，字段可原样解回。
func TestCaptureWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WB2API_CAPTURE_DIR", dir)
	Reset()
	if !Enabled() {
		t.Fatal("设了目录应启用")
	}
	Capture("stop-no-toolcall", map[string]any{"text": "现在把这两个审批单补进映射。", "tools": 8})
	Capture("stop-no-toolcall", map[string]any{"text": "第二条"})
	if Wrote() != 2 {
		t.Fatalf("应写 2 行，实际 %d", Wrote())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "stop-no-toolcall.jsonl"))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	lines := 0
	for _, line := range splitLines(raw) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v", lines+1, err)
		}
		lines++
	}
	if lines != 2 {
		t.Fatalf("应 2 行合法 JSON，实际 %d", lines)
	}
}

// TestCaptureStopsAtMaxLines 超过上限就不再写（防 26 万请求/天写满磁盘）。
func TestCaptureStopsAtMaxLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WB2API_CAPTURE_DIR", dir)
	Reset()
	for i := 0; i < MaxLines+10; i++ {
		Capture("cap", map[string]any{"i": i})
	}
	if got := Wrote(); got != MaxLines {
		t.Fatalf("应在上限 %d 处停，实际 %d", MaxLines, got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "cap.jsonl"))
	if n := len(splitLines(raw)); n != MaxLines {
		t.Fatalf("文件行数 %d want %d", n, MaxLines)
	}
}

// TestCaptureTruncatesHugeRecord 单行超长要截断，不能把 200KB 的正文整段写进去。
func TestCaptureTruncatesHugeRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WB2API_CAPTURE_DIR", dir)
	Reset()
	big := make([]byte, maxLineBytes*2)
	for i := range big {
		big[i] = 'x'
	}
	Capture("big", map[string]any{"text": string(big)})
	info, err := os.Stat(filepath.Join(dir, "big.jsonl"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > maxLineBytes+10 {
		t.Fatalf("单行 %d 字节，超过上限 %d", info.Size(), maxLineBytes)
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
