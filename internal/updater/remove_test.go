// remove_test.go 「删刚跑过的 exe」重试逻辑的回归钉。
//
// 为什么单独钉：这是本次 flake 的根因。Windows 上 WaitForSingleObject(WAIT_OBJECT_0)
// 只保证进程终止，映像 section 从文件上解绑有几十毫秒延迟，紧接着 DeleteFile 会
// ERROR_ACCESS_DENIED。不重试的后果不是「测试偶发失败」而是真机上升级回滚失败
// （服务停在新版本起不来的状态）与 helper 永久残留。
package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRemoveFileRetryingDeletesJustExecutedExe 真跑一个子进程、等它退出、立刻删它的
// 映像文件——必须成功（内部按需重试）。
//
// 曾经用裸 os.Remove 的实测结果：首次必失败、50~150ms 后一次重试即成功。
// 这条用例在旧实现下会稳定失败，所以它是根因的回归钉而不是风格测试。
func TestRemoveFileRetryingDeletesJustExecutedExe(t *testing.T) {
	updateProcessTestMu.Lock()
	defer updateProcessTestMu.Unlock()
	// 上一条进程替换测试可能让 /proc/self/exe 暂指到 .old；复制启动时的测试二进制。
	payload, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	name := "victim"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, payload, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(dst, "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		// 子进程跑 0 个测试正常退出；有错误说明环境不对，直接暴露。
		t.Fatalf("fixture: %v", err)
	}
	if err := removeFileRetrying(dst, 2*time.Second); err != nil {
		t.Fatalf("删刚执行过的 exe 失败（这正是回滚失败的原因）: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("文件仍存在: %v", err)
	}
}

// TestRemoveFileRetryingIsIdempotent 文件不存在视为成功（更新残留清扫会重复调用）。
func TestRemoveFileRetryingIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope")
	if err := removeFileRetrying(path, time.Second); err != nil {
		t.Fatalf("不存在的文件应视为成功: %v", err)
	}
}
