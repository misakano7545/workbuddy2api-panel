//go:build windows

package updater

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func setHiddenWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result, err := windows.WaitForSingleObject(handle, 250)
		if err != nil {
			return err
		}
		if result == windows.WAIT_OBJECT_0 {
			return nil
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			return errors.New("等待旧进程退出失败")
		}
	}
	return os.ErrDeadlineExceeded
}

func launchCleanup(executable, path string, pid int) {
	if executable == "" || path == "" {
		return
	}
	cmd := exec.Command(executable, cleanupArg, strconv.Itoa(pid), path)
	setHiddenWindow(cmd)
	_ = startChild(cmd)
}

func processSupervised() bool {
	managed, err := svc.IsWindowsService()
	return err == nil && managed
}
