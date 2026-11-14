package server

import (
	"log"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 11140 风控连坐保护（吸收 workbuddy2api-hub 的 restriction hold）。
//
// 上游把「账号存在异常行为，已被限制使用」（403 / code 11140，判据见 upstream/restrict.go）
// 判在设备与出口上，不是判在单个账号上。原来的处置是「禁用这个号、立刻换下一个号继续同一
// 请求」，于是一个客户端请求能顺着轮转把池子挨个撞一遍（hub #422 实测：一次任务 7 个号全封）。
//
// 两道闸：
//  1. 命中即停手：禁用该号，本请求不再换号（chatCompletions 的 HTTP 错误与首帧否决两条路径）；
//  2. 同一域、同一窗口内 ≥ restrictionWaveMin 个不同账号被限制 → 判为连坐扫荡，该域暂停派发
//     upstream.restriction_hold_seconds，期间该域的 chat 请求快速失败（503 + Retry-After）。
//
// 为什么按域（cn / global）而不是全池：两个域是两套独立的上游服务（不同域名、不同账号体系、
// 各自的风控），CN 的扫荡说明不了 global 的出口有没有被盯上；全池暂停会把一个域的风控事件
// 放大成整个网关停摆。更细的按出口（代理路由）分组需要 Handler 知道每个号的路由，
// 目前拿不到，按域是能确定成立的最小范围。
//
// 作用域：只挂 chat 主链路（11140 由 chat 返回，其余端点的流量与风险都小得多）。状态在进程内、
// 重启清零——「暂停」的意思是「现在先别打」，重启后重新判定即可。

// restrictionWaveWindow 连坐判定窗口；restrictionWaveMin 窗口内不同账号数阈值。
// 单号反复 11140 归账号自己的禁用，不触发暂停。var 只为测试能调短。
//
// 阈值取 2（与 hub 一致）而不是更高：每多等一个号，就是多一个号被永久封禁（申诉才能解），
// 而误判的代价只是该域暂停 30 分钟、可配为 0。误判要两个不同账号在 15 分钟内各自命中
// 「code 11140 + 账号专属措辞」才会凑成，单次误分类凑不出暂停。
var (
	restrictionWaveWindow = 15 * time.Minute
	restrictionWaveMin    = 2
)

type restrictionState struct {
	hits  map[string]time.Time // uid → 最近一次被限制的时刻（窗内，惰性剪枝）
	until time.Time            // 该域暂停截止；零值 = 未暂停
}

// restrictionGate 按域的连坐状态机。零值可用。
type restrictionGate struct {
	mu sync.Mutex
	m  map[string]*restrictionState
}

// note 记一次 uid 在 realm 上的账号风控限制，返回本次是否触发该域暂停。hold <= 0 表示配置
// 关掉了暂停（只保留「命中即停手」）。
//
// 同一 uid 重复命中只更新时间、不增计数（口径是「不同账号数」），一个号被反复重试不会把
// 暂停骗出来；判定成立后清空窗口，暂停解除后要全新的一轮才再触发。
func (g *restrictionGate) note(realm, uid string, hold time.Duration) bool {
	if hold <= 0 {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*restrictionState{}
	}
	st := g.m[realm]
	if st == nil {
		st = &restrictionState{hits: map[string]time.Time{}}
		g.m[realm] = st
	}
	for u, t := range st.hits {
		if now.Sub(t) > restrictionWaveWindow {
			delete(st.hits, u)
		}
	}
	st.hits[uid] = now
	if len(st.hits) < restrictionWaveMin {
		return false
	}
	st.until = now.Add(hold)
	st.hits = map[string]time.Time{}
	log.Printf("WARN: [server] upstream restriction wave realm=%q: %d accounts restricted within %s, dispatch paused %s (until %s)",
		realm, restrictionWaveMin, restrictionWaveWindow, hold, st.until.Format(time.RFC3339))
	return true
}

// remaining realm 的暂停剩余时长（0 = 未暂停，可正常派发）。
func (g *restrictionGate) remaining(realm string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.m[realm]; st != nil {
		if d := time.Until(st.until); d > 0 {
			return d
		}
	}
	return 0
}

// noteRestriction 账号处置之后调用：本次失败若是 11140 账号限制（任一形态），计入所属域的
// 连坐判定并返回 true，调用方据此停止换号——换下一个号等于把它也送进风控。
func (h *Handler) noteRestriction(acct *auth.Auth, kind upstream.ErrKind, body string) bool {
	if kind != upstream.ErrAccountFault || !upstream.IsAccountRestriction(body) {
		return false
	}
	h.restrict.note(acct.Realm(), acct.UID, h.cfg.RestrictionHold)
	return true
}
