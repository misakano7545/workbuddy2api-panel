// 账号级路由偏好（issue #62）：优先级硬分层、占比归一化分流、零配置零影响。
//
// 只测语义不测实现细节：所有断言都用公开入口（Pick/SetCredits/Cooldown）观察「谁的流量」。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// mkPool 建一个 n 号池，uid = u0..u(n-1)，余额从左到右递减（模拟"有的号积分多、有的少"）。
func mkPool(n int, credits ...int64) *Pool {
	p := New("")
	for i := 0; i < n; i++ {
		uid := "u" + string(rune('0'+i))
		p.Add(&auth.Auth{UID: uid})
		if i < len(credits) {
			p.SetCredits(uid, credits[i], 0)
		}
	}
	return p
}

// pickAll 连抽 n 次，返回各 uid 的次数。
func pickAll(p *Pool, n int) map[string]int {
	got := map[string]int{}
	for i := 0; i < n; i++ {
		if a := p.Pick(); a != nil {
			got[a.UID]++
		}
	}
	return got
}

// TestAccountPriorityIsHardLayer：配了优先级的号把流量全吃掉，哪怕余额远少于别人；
// 本层不可用（冷却）后自动让位给下一优先级层，最后才是未配置的号。
func TestAccountPriorityIsHardLayer(t *testing.T) {
	withNoPickGap(t)
	// u0 余额最多，u1/u2 少 → 没优先级时加权路由偏 u0（基线）。
	p := mkPool(3, 100000, 10, 10)
	if got := pickAll(p, 200); got["u0"] < 150 {
		t.Fatalf("基线口径异常：无优先级时余额最多的 u0 应占多数，实际 %v", got)
	}

	// u1=优先级 1（先烧），u2=优先级 2 → 只烧 u1。
	p.SetAccountPriority(map[string]int{"u1": 1, "u2": 2})
	if got := pickAll(p, 100); got["u1"] != 100 {
		t.Fatalf("优先级 1 层应独占流量，实际 %v", got)
	}
	// 状态透出（Status.Priority/Share）是「配置到底有没有匹配上」的唯一自证手段：
	// uid 打错时这里恒 0，而选号看起来"只是没生效"，极难排查。
	for _, st := range p.List() {
		switch st.UID {
		case "u0":
			if st.Priority != 0 {
				t.Fatalf("未配置的 u0 priority 应为 0，实际 %d", st.Priority)
			}
		case "u1":
			if st.Priority != 1 {
				t.Fatalf("u1 priority 应透出 1，实际 %d", st.Priority)
			}
		}
	}

	// u1 进冷却（等价于余额耗尽/停牌被 selectable 逐出）→ 让位给优先级 2 层的 u2。
	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	if got := pickAll(p, 100); got["u2"] != 100 {
		t.Fatalf("优先级 1 层不可用后应交棒给优先级 2 层，实际 %v", got)
	}

	// u2 也冷却 → 两层都没号了，回到未配置的 u0，而不是无号可用。
	p.Cooldown("u2", CoolSoft, time.Hour, "test")
	if got := pickAll(p, 50); got["u0"] != 50 {
		t.Fatalf("配置层全不可用时应回落到未配置的号，实际 %v", got)
	}
}

// TestAccountShareSplitsTrafficProportionally：同层内按 share 比例分流量（3:1），
// 且配了占比的号独占该层流量（未配的号分不到）。
func TestAccountShareSplitsTrafficProportionally(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 100000, 100000, 100000)
	p.SetAccountShare(map[string]float64{"u0": 3, "u1": 1})

	const N = 8000
	got := pickAll(p, N)
	if got["u2"] != 0 {
		t.Fatalf("配了占比的号应独占本层，未配的 u2 不应分到流量，实际 %v", got)
	}
	// 期望 u0:u1 = 3:1 = 6000:2000。二项分布 σ≈39，容差 ±5%（400）≈ 10σ，不会偶发抖动。
	if want, tol := N*3/4, N/20; got["u0"] < want-tol || got["u0"] > want+tol {
		t.Fatalf("占比 3:1 应分出约 %d/%d 的流量，实际 %v", want, N-want, got)
	}
}

// TestAccountShareBeatsPreferExpiring：占比优先于「最早到期优先」——后者是从全候选硬选
// 一个号，若排在占比之后，占比就永远不生效（issue #62 讨论里点的次序问题）。
func TestAccountShareBeatsPreferExpiring(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(2, 10, 10)
	now := time.Now()
	// u0 有一批 1 小时后到期，u1 没有 → 默认 prefer_expiring=true 时 u0 本该被优先选。
	p.SetCreditsDetailed("u0", 10, 10, 10, now.Add(time.Hour), 10)
	p.SetAccountShare(map[string]float64{"u1": 1})
	if got := pickAll(p, 50); got["u1"] != 50 {
		t.Fatalf("配了占比的号应独占流量（压过到期优先），实际 %v", got)
	}
}

// TestNoAccountPrefsIsUnchanged：空表（或非正值被剔除后为空）时选号行为与不开此功能一致。
func TestNoAccountPrefsIsUnchanged(t *testing.T) {
	withNoPickGap(t)
	// 注入源固定返回 0 → 命中权重表首位的账号，结果完全确定，可直接比对两个池。
	build := func(p *Pool) *Pool {
		p.Add(&auth.Auth{UID: "u0"})
		p.Add(&auth.Auth{UID: "u1"})
		p.SetCredits("u0", 100, 0)
		p.SetCredits("u1", 50, 0)
		p.SetRandomSource(func(n int64) int64 { return 0 })
		return p
	}
	base := build(New(""))
	withEmpty := build(New(""))
	withEmpty.SetAccountPriority(map[string]int{})
	withEmpty.SetAccountShare(map[string]float64{})
	for i := 0; i < 20; i++ {
		a, b := base.Pick(), withEmpty.Pick()
		if a == nil || b == nil || a.UID != b.UID {
			t.Fatalf("iter %d: 空配置池选号应与非空配置池完全一致：%v vs %v", i, a, b)
		}
	}
}
