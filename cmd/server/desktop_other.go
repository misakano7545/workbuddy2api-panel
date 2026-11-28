//go:build !windows

package main

import (
	"context"
	"io"
	"os"
)

// prepareDesktopUI 在非 Windows 上没有控制台窗口需要隐藏，日志保持原有输出。
func prepareDesktopUI() {}

func desktopLogTargets(extra io.Writer) []io.Writer {
	return []io.Writer{os.Stderr, extra}
}

// startDesktop 在非 Windows 上没有内置托盘，仍按同一入口自动打开控制台。
// 设置 WB2A_NO_BROWSER=1 可关闭自动打开。
func startDesktop(_ context.Context, _ context.CancelFunc, panelURL string, _ desktopStatsFunc) {
	openBrowser(panelURL)
}
