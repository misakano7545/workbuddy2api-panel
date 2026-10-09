package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 平滑紧迫度梯度（灵感：上游 issue #140）。
//
// 固定倍率下「还剩 6 天」与「还剩 6 小时」权重完全相等，命悬一线的号拿不到优先级。
// 梯度要求：同一窗口内越逼近到期倍率越大，且软偏好语义不变（倍率有上界，不会把流量
// 全压到单一账号）。
func TestExpiringUrgencyGradient(t *testing.T) {
	const w = 168 * time.Hour
	now := time.Now()
	p := New("")
	for _, uid := range []string{"near", "mid", "far", "none"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetPreferExpiring(true, w)
	// near=还剩 1h（逼近到期）、mid=还剩 84h（窗口中点）、far=还剩 167h（窗口边缘）、
	// none=有余额但无有效快过期批次。
	p.SetCreditsDetailed("near", 100, 100, 100, now.Add(time.Minute), 100)
	p.SetCreditsDetailed("mid", 100, 100, 100, now.Add(84*time.Hour), 100)
	p.SetCreditsDetailed("far", 100, 100, 100, now.Add(167*time.Hour), 100)
	p.SetCreditsDetailed("none", 100, 100, 0, time.Time{}, 0)

	p.mu.Lock()
	near := p.expiringUrgency(p.byUID["near"], now)
	mid := p.expiringUrgency(p.byUID["mid"], now)
	far := p.expiringUrgency(p.byUID["far"], now)
	none := p.expiringUrgency(p.byUID["none"], now)
	wNear := p.routingWeightOf(p.byUID["near"], 100, now)
	wNone := p.routingWeightOf(p.byUID["none"], 100, now)
	p.mu.Unlock()

	if none != 1 {
		t.Fatalf("无快过期批次应不加权，得到 %v", none)
	}
	if wNear <= wNone {
		t.Fatalf("快过期账号路由权重应更高: %v vs %v", wNear, wNone)
	}
	if !(near > mid && mid > far) {
		t.Fatalf("紧迫度应随剩余时间单调递减: near=%v mid=%v far=%v", near, mid, far)
	}
	if near > expiringUrgencyMax || far < expiringUrgencyMin {
		t.Fatalf("倍率越界 [%v,%v]: near=%v far=%v", expiringUrgencyMin, expiringUrgencyMax, near, far)
	}
	// 距窗口两端 5% 以内即算「边缘/逼近到期」（留出两次取时钟之间的毫微漂移）。
	band := (expiringUrgencyMax - expiringUrgencyMin) * 0.05
	if far > expiringUrgencyMin+band {
		t.Fatalf("窗口边缘应≈%v，得到 %v", expiringUrgencyMin, far)
	}
	if near < expiringUrgencyMax-band {
		t.Fatalf("逼近到期应≈%v，得到 %v", expiringUrgencyMax, near)
	}
	// sqrt 形状：窗口中点的紧迫度**低于**算术中点——「尾部才陡」正是它能压过 credits 基数的
	// 原因（改回线性 T/W 会让 near/mid/far 挤成一团，本条即红）。
	if arith := (expiringUrgencyMin + expiringUrgencyMax) / 2; mid >= arith {
		t.Fatalf("窗口中点应明显低于算术中点 %v（尾部陡升形状），得到 %v", arith, mid)
	}
}

// issue #140 第 2 点的验收：**快到期的低余额号压过下周到期的高余额号**。
// 两号闲置状态相同（都「刚用过」，无闲置补偿）以隔离出 credits 基数 vs 紧迫度这一对变量：
// 高余额号余量 500（池内最大 → credits 项拿满 10）、剩 6 天；低余额号余量 50、剩 1 天。
func TestExpiringUrgencyFlipsHighCreditAccount(t *testing.T) {
	const w = 168 * time.Hour
	now := time.Now()
	p := New("")
	p.Add(&auth.Auth{UID: "hi"})
	p.Add(&auth.Auth{UID: "lo"})
	p.SetPreferExpiring(true, w)
	p.SetCreditsDetailed("hi", 500, 500, 500, now.Add(6*24*time.Hour), 500)
	p.SetCreditsDetailed("lo", 50, 50, 50, now.Add(24*time.Hour), 50)

	p.mu.Lock()
	p.byUID["hi"].lastUsed = now // 两号都刚用过 → 闲置项同为 0，只剩 credits 项之差
	p.byUID["lo"].lastUsed = now
	hi := p.routingWeightOf(p.byUID["hi"], 500, now)
	lo := p.routingWeightOf(p.byUID["lo"], 500, now)
	p.mu.Unlock()

	if lo <= hi {
		t.Fatalf("快到期的低余额号应压过高余额号: lo=%v hi=%v", lo, hi)
	}
	// 不是「勉强高一点」：差距要够大才算「压过」（余额差 10 倍、到期差 6 天，实测 ≈1.8×）。
	if lo <= hi*1.5 {
		t.Fatalf("压过幅度过小（应 ≥1.5×）: lo=%v hi=%v", lo, hi)
	}
}

// 未注入窗口（嵌入方未调 SetPreferExpiring 的第二参）退回旧的固定倍率；0 与负值同理，
// 且不得算出 <1 的倍率（那会惩罚快过期账号）。
func TestExpiringUrgencyWithoutWindowFallsBack(t *testing.T) {
	now := time.Now()
	for _, w := range []time.Duration{0, -time.Hour} {
		p := New("")
		p.Add(&auth.Auth{UID: "a"})
		p.SetPreferExpiring(true, w)
		p.SetCreditsDetailed("a", 100, 100, 100, now.Add(time.Hour), 100)

		p.mu.Lock()
		got := p.expiringUrgency(p.byUID["a"], now)
		p.mu.Unlock()
		if got != expiringVirtualSlots {
			t.Fatalf("window=%v 应退回固定倍率 %v，得到 %v", w, expiringVirtualSlots, got)
		}
	}
}

// 新会话候选集（realm.go 的虚拟实例数）与 pick.go 的路由权重吃同一份梯度口径：
// 临近到期 5 个实例、窗口边缘 2 个（round(1.5)），两者都不再是恒 3。
func TestExpiringUrgencyGradientInRealmSlots(t *testing.T) {
	const w = 168 * time.Hour
	now := time.Now()
	p := New("")
	p.Add(&auth.Auth{UID: "near", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "far", Domain: "www.codebuddy.cn"})
	p.SetPreferExpiring(true, w)
	p.SetCreditsDetailed("near", 100, 100, 100, now.Add(time.Minute), 100)
	p.SetCreditsDetailed("far", 100, 100, 100, now.Add(w-time.Hour), 100)

	count := map[string]int{}
	for _, uid := range p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn") {
		count[uid]++
	}
	if count["near"] != int(expiringUrgencyMax) {
		t.Fatalf("临近到期应出现 %v 次，得到 %d", expiringUrgencyMax, count["near"])
	}
	if count["far"] != 2 {
		t.Fatalf("窗口边缘应出现 2 次，得到 %d", count["far"])
	}
}
