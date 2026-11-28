package main

import (
	"log"
	"os"
	"os/exec"
	"runtime"
)

// desktopStats 是托盘右键菜单顶部的只读统计行，由主程序按当前数据实时生成。
type desktopStats struct {
	CreditsLine string
	TodayLine   string
}

type desktopStatsFunc func() desktopStats

// openBrowser 用系统默认浏览器打开控制台。WB2A_NO_BROWSER=1 时只保留托盘
// （或纯服务）行为，方便无桌面环境和自动化测试使用。
func openBrowser(rawURL string) {
	if os.Getenv("WB2A_NO_BROWSER") != "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("WARN: 自动打开控制台失败，请手动访问 %s: %v", rawURL, err)
		return
	}
	go func() { _ = cmd.Wait() }()
}
