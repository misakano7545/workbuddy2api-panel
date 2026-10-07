package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 计数口径：paused 单列 vs 并入 disabled——同一份遍历两种口径都要成立。
//
// 为什么：/status（与 /metrics）的「不可用」= disabled + paused 是监控契约，不能因面板
// 展示需要而改变；面板概况要分开（禁用几个、暂停几个）。吸收上游 PR #130 的修法后，
// 共享遍历不再替调用方决定合并与否。
func TestCountsPausedSplitAndMerged(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.Disable("u2", "test disable")
	if !p.Pause("u3") {
		t.Fatal("Pause 失败")
	}

	// ① 合并口径（/status、/metrics）：两个都算进 disabled。
	total, _, _, disabled, _ := p.CountsDetailed()
	if total != 3 || disabled != 2 {
		t.Errorf("CountsDetailed: total=%d disabled=%d want 3/2（禁用 + 暂停都算不可用）", total, disabled)
	}
	// ② 分开口径（面板概况）：禁用 1、暂停 1。
	_, _, _, d2, pz, _ := p.CountsDetailedWithPaused()
	if d2 != 1 || pz != 1 {
		t.Errorf("CountsDetailedWithPaused: disabled=%d paused=%d want 1/1", d2, pz)
	}
	// ③ 按域口径保持合并语义不变。
	_, _, _, d3, _ := p.CountsDetailedForRealm("cn")
	if d3 != 2 {
		t.Errorf("CountsDetailedForRealm: disabled=%d want 2（口径与 CountsDetailed 一致）", d3)
	}
	// ④ 遍历本身：RealmHealth 里两者互斥、且 healthy/cooling 不受影响。
	h := p.RealmHealth("")
	if h.Disabled != 1 || h.Paused != 1 || h.Healthy != 1 {
		t.Errorf("RealmHealth: disabled=%d paused=%d healthy=%d want 1/1/1", h.Disabled, h.Paused, h.Healthy)
	}
	// ⑤ 恢复暂停号后回到「可用」，数字逐位回退。
	p.Resume("u3")
	_, _, _, d4, pz4, _ := p.CountsDetailedWithPaused()
	if d4 != 1 || pz4 != 0 {
		t.Errorf("Resume 后 disabled=%d paused=%d want 1/0", d4, pz4)
	}
}
