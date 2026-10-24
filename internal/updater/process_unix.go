//go:build !windows

package updater

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

func setHiddenWindow(_ *exec.Cmd) {}

func waitForProcessExit(pid int, timeout time.Duration) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return os.ErrDeadlineExceeded
}

func launchCleanup(_, _ string, _ int) {}

func processSupervised() bool {
	if runtime.GOOS == "linux" {
		return os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != "" || os.Getenv("RC_SVCNAME") != ""
	}
	return runtime.GOOS == "darwin" && os.Getenv("LAUNCH_JOBKEY_LABEL") != ""
}
