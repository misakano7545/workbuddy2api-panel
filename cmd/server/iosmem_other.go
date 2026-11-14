//go:build !darwin

package main

// 非 darwin 平台（本机 go test / CI 的 linux 步骤）只是占位：
// memorystatus_control / proc_info 都是 XNU 私有系统调用。

func applyProcessLimits() string { return "非 darwin，跳过 jetsam 上限" }

func maxRSSMB() int { return -1 }

func memSample() (int, int) { return -1, -1 }

func logBootMemory() {}

func logJetsamEvidence() {}
