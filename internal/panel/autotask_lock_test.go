package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestTaskAccountLockSameAccountExclusive 同一账号的任务锁互斥：第二次 tryLock 必须失败，
// 解锁后可再次获取。这是「重复点一键完成不并发重跑」的核心保障。
func TestTaskAccountLockSameAccountExclusive(t *testing.T) {
	p := &Panel{}
	uid := "u1"

	if !p.tryLockAccount(uid) {
		t.Fatal("首次加锁应成功")
	}
	if p.tryLockAccount(uid) {
		t.Fatal("同账号第二次加锁应失败（互斥）")
	}
	p.unlockAccount(uid)

	if !p.tryLockAccount(uid) {
		t.Fatal("解锁后应可再次加锁")
	}
	p.unlockAccount(uid)
}

func TestFirstBuddyNotSkippedWhenProgressFull(t *testing.T) {
	open := &upstream.Task{TaskCode: "first_buddy", Current: 1, Target: 1, AcceptStatus: "completed"}
	if growthActionSkipped(open) {
		t.Fatal("进度满但未领取，不能跳过")
	}
	open.Claimed = true
	if !growthActionSkipped(open) {
		t.Fatal("已领取应跳过")
	}
	other := &upstream.Task{TaskCode: "chat_5", Current: 5, Target: 5}
	if !growthActionSkipped(other) {
		t.Fatal("其它任务进度满仍跳过")
	}
}

// TestTaskAccountLockDifferentAccountsIndependent 不同账号的锁互不影响（并行照旧）。
func TestTaskAccountLockDifferentAccountsIndependent(t *testing.T) {
	p := &Panel{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 加锁应成功")
	}
	if !p.tryLockAccount("u2") {
		t.Fatal("u2 加锁应成功（不同账号不互斥）")
	}
	p.unlockAccount("u2")
	p.unlockAccount("u1")
}

// TestTaskAccountLockCrossEntryShared 单任务 auto 与全量 auto_all 共用同一把账号锁
// （在 handler 层都走 tryLockAccount，这里验证锁命名空间一致）。
func TestTaskAccountLockCrossEntryShared(t *testing.T) {
	p := &Panel{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 加锁应成功")
	}
	// 模拟全量入口对同一 uid 加锁——必须被挡（否则两入口可并发）。
	if p.tryLockAccount("u1") {
		t.Fatal("同 uid 跨入口加锁应失败（共用锁）")
	}
	p.unlockAccount("u1")
}

func TestGrowthPendingKeepsUnclaimed(t *testing.T) {
	met := upstream.Task{TaskCode: "Sequential_Tasks_3", Target: 5, Current: 5}
	if !growthPending(met) {
		t.Fatal("达标未领应入队")
	}
	met.Claimed = true
	if growthPending(met) {
		t.Fatal("已领不应入队")
	}
	if growthPending(upstream.Task{TaskCode: "Sequential_Tasks_4", Locked: true}) {
		t.Fatal("锁定不应入队")
	}
}

func TestMPChatTarget(t *testing.T) {
	if mpChatTarget("Sequential_Tasks_3", 0) != 5 {
		t.Fatal("空进度 Tasks_3 应为 5")
	}
	if mpChatTarget("Sequential_Tasks_3", 5) != 5 || mpChatTarget("Sequential_Tasks_1", 0) != 1 {
		t.Fatal("已下发 target 优先，Tasks_1 空进度为 1")
	}
	if mpChatTarget("Sequential_Tasks_6", 0) != 10 {
		t.Fatal("空进度 Tasks_6 应为 10")
	}
}
