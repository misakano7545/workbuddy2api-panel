package updater

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestScheduleWindowToleratesMissedMinute 自动更新改窗口式判定的回归：
// 分钟精确相等在 ticker 相位漂移 / Check 占住循环跨过目标分钟 / 进程晚启动时
// 会让当天自动更新静默落空；窗口式（今天没跑过 且 已到/过目标时刻）能补跑。
func TestScheduleWindowToleratesMissedMinute(t *testing.T) {
	m := New("dev", "", "", Settings{AutoEnabled: true, AutoTime: "04:00"})
	// 03:59（上海）未到点 → 不触发
	before := time.Date(2026, 10, 23, 19, 59, 0, 0, time.UTC)
	if auto, _ := m.scheduleDue(before); auto {
		t.Fatal("未到自动时间不应触发")
	}
	// 04:07 才第一次评估（错过目标分钟）→ 仍应补跑当天一次
	late := time.Date(2026, 10, 23, 20, 7, 0, 0, time.UTC)
	if auto, _ := m.scheduleDue(late); !auto {
		t.Fatal("错过目标分钟的当天应补跑")
	}
	if auto, _ := m.scheduleDue(late.Add(time.Minute)); auto {
		t.Fatal("同一天只跑一次")
	}
}

// TestDownloadStallDetected 下载卡死守卫：连接活着但 N 秒无新字节必须报错，
// 而不是依赖 30s 整体超时（大包慢网会被整体超时误杀，这里钉的是另一半——
// 「慢但在动」允许、「彻底不动」快速失败）。
func TestDownloadStallDetected(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("partial"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block // 模拟上游发了一半后卡死
	}))
	defer server.Close()
	defer close(block)

	m := New("dev", "", "", Settings{})
	old := downloadStallWindow
	downloadStallWindow = 200 * time.Millisecond
	defer func() { downloadStallWindow = old }()

	start := time.Now()
	err := m.downloadAsset(context.Background(), &releaseAsset{BrowserDownloadURL: server.URL}, io.Discard)
	if err == nil {
		t.Fatal("卡死的下载应在守卫窗口内报错")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("卡死守卫应快速失败, elapsed=%v", elapsed)
	}
}

// TestInstallWatchdogResetsStuckInstalling Install 成功只代表 helper 被拉起；
// helper 后续失败时看门狗要复位 Installing 并留下原因，面板可重试而不是永久
// 「更新已在进行中」。
func TestInstallWatchdogResetsStuckInstalling(t *testing.T) {
	m := New("dev", "", "", Settings{})
	m.SetRestart(func() {})
	old := installWatchdog
	installWatchdog = 40 * time.Millisecond
	defer func() { installWatchdog = old }()

	m.status.Installing = true
	m.armInstallWatchdog()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.Status().Installing {
		time.Sleep(10 * time.Millisecond)
	}
	st := m.Status()
	if st.Installing {
		t.Fatal("看门狗应复位 Installing")
	}
	if st.LastError == "" {
		t.Fatal("复位应留下 LastError 说明原因")
	}
}

// TestValidateHelperPath 私有子命令的文件参数收敛：同目录 + .wb2api- 前缀，
// 目标二进制只要求同目录。cleanup 入口此前可删任意路径的任意文件。
func TestValidateHelperPath(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(self)
	if err := validateHelperPath(filepath.Join(dir, ".wb2api-stage-123"), ".wb2api-"); err != nil {
		t.Fatalf("同目录带前缀应通过: %v", err)
	}
	if err := validateHelperPath(filepath.Join(dir, "config.json"), ".wb2api-"); err == nil {
		t.Fatal("无前缀文件应被拒绝")
	}
	if err := validateHelperPath(filepath.Join(t.TempDir(), ".wb2api-stage-x"), ".wb2api-"); err == nil {
		t.Fatal("程序目录之外的文件应被拒绝")
	}
	if err := validateHelperPath(filepath.Join(dir, "wb2api"), ""); err != nil {
		t.Fatalf("目标二进制只要求同目录: %v", err)
	}
}

// TestCleanupStaleTempFiles 启动清扫：只清超过阈值的 .wb2api-* 残留临时文件，
// 新文件与回滚备份 .old 一律不动。
func TestCleanupStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".wb2api-stage-old")
	fresh := filepath.Join(dir, ".wb2api-updater-new")
	rollback := filepath.Join(dir, "wb2api.old")
	for _, p := range []string{stale, fresh, rollback} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(stale, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	CleanupStaleTempFiles(dir, time.Hour)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("过期残留应被清扫")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("未过阈值的文件不应被清扫")
	}
	if _, err := os.Stat(rollback); err != nil {
		t.Fatal("回滚备份 .old 不应被清扫")
	}
}

// TestWaitChildHealthy 健康观察窗口：窗口内退出的子进程报错（调用方据此回滚），
// 活过窗口返回 nil。用「重跑自身测试二进制」的经典 helper-process 手法造子进程。
func TestWaitChildHealthy(t *testing.T) {
	switch os.Getenv("WB2API_UPDATER_CHILD") {
	case "exit":
		return // 立即退出
	case "sleep":
		time.Sleep(10 * time.Second)
		return
	}
	updateProcessTestMu.Lock()
	defer updateProcessTestMu.Unlock()

	exited := exec.Command(os.Args[0], "-test.run", "^TestWaitChildHealthy$")
	exited.Env = append(os.Environ(), "WB2API_UPDATER_CHILD=exit")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitChildHealthy(exited, 2*time.Second); err == nil {
		t.Fatal("窗口内退出的子进程不应通过健康检查")
	}

	alive := exec.Command(os.Args[0], "-test.run", "^TestWaitChildHealthy$")
	alive.Env = append(os.Environ(), "WB2API_UPDATER_CHILD=sleep")
	if err := alive.Start(); err != nil {
		t.Fatal(err)
	}
	defer alive.Process.Kill()
	if err := waitChildHealthy(alive, 300*time.Millisecond); err != nil {
		t.Fatalf("存活子进程应通过健康窗口: %v", err)
	}
}
