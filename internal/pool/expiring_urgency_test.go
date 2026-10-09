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
	p.SetCreditsDetailed("near", 100, 100, 100, now.Add(time.Hour), 100)
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
	if want := (expiringUrgencyMin + expiringUrgencyMax) / 2; mid < want-band || mid > want+band {
		t.Fatalf("窗口中点应≈%v，得到 %v", want, mid)
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
