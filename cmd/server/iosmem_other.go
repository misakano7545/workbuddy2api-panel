//go:build !darwin

package main

// 非 darwin 平台（本机 go test / CI 的 linux 步骤）只是占位：
// memorystatus_control 是 XNU 私有系统调用，其它平台没有这个东西。

func applyProcessLimits() string { return "非 darwin，跳过 jetsam 上限" }

func maxRSSMB() int { return -1 }

func logJetsamEvidence() {}
