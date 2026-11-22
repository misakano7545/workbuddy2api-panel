//go:build windows

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	desktopLogPrefix        = "wb2api-"
	desktopLogSuffix        = ".log"
	desktopLogDayLayout     = "2006-01-02"
	desktopLogRetentionDays = 7
)

// dailyLogWriter 按本地日期切分桌面日志，并在每次换日时把超过 7 天的旧文件删掉。
// 日志和账号凭证同处 %APPDATA%\workbuddy2api，权限沿用用户目录的 0700。
type dailyLogWriter struct {
	mu   sync.Mutex
	dir  string
	file *os.File
	day  string
}

var desktopLogWriter *dailyLogWriter

func newDailyLogWriter(dir string) *dailyLogWriter {
	return &dailyLogWriter{dir: dir}
}

func (w *dailyLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	day := time.Now().Format(desktopLogDayLayout)
	if w.file == nil || day != w.day {
		if w.file != nil {
			_ = w.file.Close()
			w.file = nil
		}
		path := filepath.Join(w.dir, desktopLogPrefix+day+desktopLogSuffix)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		w.file = f
		w.day = day
		w.prune()
	}
	return w.file.Write(p)
}

// Close 仅在测试或显式关停时使用，正常退出由进程回收句柄。
func (w *dailyLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// prune 只处理 wb2api-YYYY-MM-DD.log，按日期排序保留最近 7 个（含当天），
// 更旧的直接删除；不碰目录里的其他文件（auths、panel-logs 等）。
func (w *dailyLogWriter) prune() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	nameLen := len(desktopLogPrefix) + len(desktopLogDayLayout) + len(desktopLogSuffix)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) != nameLen || !strings.HasPrefix(name, desktopLogPrefix) || !strings.HasSuffix(name, desktopLogSuffix) {
			continue
		}
		datePart := name[len(desktopLogPrefix) : len(desktopLogPrefix)+len(desktopLogDayLayout)]
		if _, err := time.Parse(desktopLogDayLayout, datePart); err == nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) <= desktopLogRetentionDays {
		return
	}
	for _, name := range names[:len(names)-desktopLogRetentionDays] {
		_ = os.Remove(filepath.Join(w.dir, name))
	}
}
