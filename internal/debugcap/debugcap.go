// Package debugcap 一次性取证落盘：把线上「只能靠猜」的形态抓成 JSONL。
//
// 定位方式：默认**完全不启用**（不设 WB2API_CAPTURE_DIR 就没有任何 IO、没有额外
// 分配）。设了目录之后，只有调用方显式命中的形态才会写一行——不是全量采样，
// 否则 26 万请求/天会直接把磁盘写满。
//
// 为什么要这个：网关只记元数据不存 body，而「模型说一句就停」这种形态无法从
// 元数据判断是上游行为、客户端 tool_choice、还是网关改写造成的。抓一次真实
// 请求的正文与出站形态，比在生产上反复试参数快得多，也可复现。
//
// 硬上限：每个 tag 最多 MaxLines 行、单行最长 maxLineBytes、整个捕获目录超过
// maxDirBytes 就停止（并记一条日志）。取证是诊断，不允许影响业务。
package debugcap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	mu       sync.Mutex
	lines    = map[string]int{}
	wrote    int64
	disabled atomic.Bool
)

// MaxLines 每个 tag 最多写多少行（够复现就行）。
const MaxLines = 40

const (
	maxLineBytes = 8 << 10  // 单行 8KB
	maxDirBytes  = 32 << 20 // 整个目录 32MB
)

// Enabled 报告取证是否启用（未设 WB2API_CAPTURE_DIR → false）。
func Enabled() bool { return os.Getenv("WB2API_CAPTURE_DIR") != "" && !disabled.Load() }

// Capture 把 rec 序列化后追加到 <dir>/<tag>.jsonl。任何失败都静默（取证不该
// 影响业务），并在超过上限时永久关闭。
func Capture(tag string, rec any) {
	dir := os.Getenv("WB2API_CAPTURE_DIR")
	if dir == "" || disabled.Load() {
		return
	}
	if tag == "" {
		tag = "misc"
	}
	mu.Lock()
	defer mu.Unlock()
	if lines[tag] >= MaxLines {
		return
	}
	if size := dirBytes(dir); size > maxDirBytes {
		disabled.Store(true)
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if len(raw) > maxLineBytes {
		raw = raw[:maxLineBytes]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, tag+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return
	}
	lines[tag]++
	wrote++
}

// dirBytes 目录总大小（只看一层，够用了：所有文件都直接写在该目录下）。
func dirBytes(dir string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range ents {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Wrote 返回已写行数（测试与日志用）。
func Wrote() int64 { return atomic.LoadInt64(&wrote) }

// Reset 清空计数与熔断（仅测试用）。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	lines = map[string]int{}
	wrote = 0
	disabled.Store(false)
}

// Now 统一时间格式（与 reqlog 一致，便于对齐）。
func Now() string { return time.Now().Format(time.RFC3339Nano) }
