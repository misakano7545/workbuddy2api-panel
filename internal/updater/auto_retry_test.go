package updater

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// issue #274 现象 1：未认证的 GitHub API 只有 60 次/小时且按出口 IP 共享，
// 被同机其它工具吃光时返回 403。原先是「一次失败就整天不再检查」——lastAutoDay
// 在 scheduleDue 里就置位了，Check 失败也算跑过。改成失败排一次 30 分钟后的重试。
func TestAutoRetryAfterCheckFailure(t *testing.T) {
	m := New("dev", "", "", Settings{AutoEnabled: true, AutoTime: "04:00"})
	due := time.Date(2026, 10, 23, 20, 7, 0, 0, time.UTC) // 上海 10-24 04:07
	if auto, _ := m.scheduleDue(due); !auto {
		t.Fatal("前提失败：到点应触发")
	}
	m.now = func() time.Time { return due }
	m.deferAuto() // 模拟 Check 拿到 403
	if auto, _ := m.scheduleDue(due.Add(time.Minute)); auto {
		t.Fatal("刚失败的一分钟内不该重试（会把限额耗得更狠）")
	}
	retry := due.Add(autoRetryDelay)
	if auto, _ := m.scheduleDue(retry); !auto {
		t.Fatal("失败后应隔 autoRetryDelay 重试，而不是等到明天")
	}
	// 走通之后当天收工。
	m.now = func() time.Time { return retry }
	m.markAutoDone()
	if auto, _ := m.scheduleDue(retry.Add(time.Minute)); auto {
		t.Fatal("走通后当天不该再跑")
	}
}

// issue #274 现象 2：下载失败会留下 0 字节的 .wb2api-release-*.zip，
// 名字看着像「已下好的更新包」。
func TestFailedDownloadLeavesNoTempFile(t *testing.T) {
	m := New("2026.11.10.1200", "o/r", "", Settings{})
	m.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: lookup github.com: no such host")
	})
	dir := filepath.Dir(m.executable)
	before, _ := filepath.Glob(filepath.Join(dir, ".wb2api-release-*"))

	asset := &releaseAsset{
		Name:               "x.zip",
		BrowserDownloadURL: "https://github.com/o/r/releases/download/v1/f.zip",
		Size:               10,
		Digest:             "sha256:" + strings.Repeat("0", 64),
	}
	if _, _, err := m.fetchAsset(context.Background(), asset); err == nil {
		t.Fatal("下载失败应返回错误")
	}
	after, _ := filepath.Glob(filepath.Join(dir, ".wb2api-release-*"))
	if len(after) > len(before) {
		t.Fatalf("下载失败留下了临时文件：%v", after)
	}
}

// 启动清扫：0 字节的残留不问年龄一律清（在途更新不可能跨进程启动存在）。
func TestCleanupRemovesEmptyTempFiles(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, ".wb2api-release-999.zip")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	CleanupStaleTempFiles(dir, time.Hour)
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("0 字节残留应被清扫")
	}
}
