//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDailyLogWriterWritesTodayFile(t *testing.T) {
	dir := t.TempDir()
	w := newDailyLogWriter(dir)
	defer w.Close()

	if _, err := w.Write([]byte("hello desktop log\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	name := desktopLogPrefix + time.Now().Format(desktopLogDayLayout) + desktopLogSuffix
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if !strings.Contains(string(raw), "hello desktop log") {
		t.Fatalf("unexpected log content: %q", raw)
	}
}

func TestDailyLogWriterPrunesOldestFiles(t *testing.T) {
	dir := t.TempDir()
	for day := 1; day <= 9; day++ {
		name := fmt.Sprintf("%s2026-01-%02d%s", desktopLogPrefix, day, desktopLogSuffix)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := newDailyLogWriter(dir)
	w.prune()

	for day := 1; day <= 2; day++ {
		name := fmt.Sprintf("%s2026-01-%02d%s", desktopLogPrefix, day, desktopLogSuffix)
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("old log %s should be removed, stat err=%v", name, err)
		}
	}
	for day := 3; day <= 9; day++ {
		name := fmt.Sprintf("%s2026-01-%02d%s", desktopLogPrefix, day, desktopLogSuffix)
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("recent log %s should be kept: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated.txt")); err != nil {
		t.Fatalf("unrelated file should be kept: %v", err)
	}
}
