// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// costExploreInterval costTier 条件探索窗口（issue #136 方案 a′，SetCostExploreInterval
	// 注入；默认 defaultCostExploreInterval 30m）。tier 0 垄断 + tier 1 存在且距上次
	// 探索 ≥ 窗口时，本次 pick 生效层切 tier 1-only（探索=搭车改道，零新增上游请求）。
	// 0 = 关停（完全回到现状行为）。
	costExploreInterval time.Duration
	// exploreLast 各 (realm, 模型) 的上次探索时刻，键 = realm + "\x1f" + model。
	// 运行态（不持久化，同 lastUsed/usedSeq 口径）：重启归零 → 每个仍冻结的
	// (域, 模型) 多至 1 次即时重探；已毕业号经 ModelCosts 恢复 tier，学费不重付。
	// 只在探索事件时写入（tier 1 枯竭期间停走，陈旧无害）；不做对称清理。
	exploreLast map[string]time.Time
	// costExploreEvents 累计探索事件数（/status 透出；pick 写锁内 ++，无需 atomic）。
	costExploreEvents int64
	// egress 出口级瞬时失败计数（egress.go，键 = 出口身份）。运行态，不持久化：
	// 链路状态易失，重启后从零开始是正确的。
	egress map[string]egressStat
	// windows 额度窗口登记表（window.go）：429 带重置时刻时登记，到点主动发探针
	// 让新一轮窗口从重置那一刻起算。**持久化**——进程内冷却表是易失的，窗口记录
	// 跟着丢就等于每次重启都错过重置时刻，正是本机制要治的那段空转。
	windows map[string]*ModelWindow
	// degradeThreshold / degradeCooldown / degradeCooldownMax 连败降权参数
	// （SetDegrade 注入；默认值见 defaultDegrade*，issue #114）。
	degradeThreshold   int
	degradeCooldown    time.Duration
	degradeCooldownMax time.Duration
	// creditFloor 积分保底（SetCreditFloor 注入；0 = 关闭，缺省即现状）。
	// 账号 credits < floor 时不再参与选号——防止收费请求把余额打穿、连免费模型都
	// 402 冷却到次日签到。签到回血（SetCreditsDetailed）越过 floor 即自动恢复。
	// 全池触底且无免费模型可接时选号返回 nil（硬语义：宁 503 不打穿）。
	// 收费与否的判据见 floorBlockedForModel：本地实测台账优先，缺失时用上游目录
	// 倍率（modelRateOf 注入）兜底，避免「无观测的高价新模型」绕过保底。
	creditFloor int64
	// creditFloorMaxRate 保底兜底允许的目录倍率上限（SetCreditFloorMaxRate 注入）。
	// 语义：账本无观测（tier 1）时，目录倍率 ≤ 该值 → 放行（让便宜的新模型有机会
	// 跑出第一笔实测入账）；> 该值 → 仍按收费模型拦（保住 kimi-k3-1 那类事故的防护）。
	// 0 = 严格模式（任何已知正倍率都拦，即 PR #2 之前的现状）；负值非法保留原值。
	creditFloorMaxRate float64
	// modelRateOf 按 (realm, 模型) 查上游目录积分倍率（"0.79" / "" = 未知）。
	// 由 main 用 upstream.Client.ModelRate 注入——pool 不依赖 upstream 包（避免
	// 循环依赖与分层破坏），nil 时倍率兜底不生效（退化为仅本地台账判定）。
	// 仅在持 p.mu 时由 floorBlockedForRealmModel 调用；回调不得反向调用 Pool 方法。
	modelRateOf func(realm, model string) string
	// 加权路由的闲置补偿调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// expiringWindow 快过期窗口（pool.expiring_soon，main 注入）。0 = 未注入 → 紧迫度
	// 退回旧的固定倍率（见 expiringUrgency）。
	expiringWindow time.Duration
	// preferExpiring 最早到期优先路由开关（默认 true）。开启且快过期窗口内存在有效
	// 批次时，选号在成本层内先按最早到期排序；关闭后只使用普通加权路由。
	preferExpiring bool
	// balanceWindow 窗口用量调度窗口（issue #112，SetBalanceWindow 注入；默认 1h，
	// 0 = 关闭）。按 (账号, 模型) 在本窗口内累计的 token 反比降权：只靠 credits
	// 加权时余额掉几分与烧掉百万 token 在权重上几乎无差别，热点号会一路烧到 6004
	// 限额而同期其他号闲置。软降权，不排除（保底/优先级/占比仍是硬语义，不受它影响）。
	balanceWindow time.Duration
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// maxInFlightGlobal global 域单账号在途上限分档（WAF 403 修复 P1-1：global 域
	// WAF 风控更紧，压低并发）；0 = 未设置，回落 maxInFlight（不分档，零回归）。
	maxInFlightGlobal int
	// reserveCredits 保留积分阈值（SetReserveCredits 注入；0 = 关闭）。余额已知
	// （creditsTotal > 0）且 credits <= 阈值 的账号停止接单——仍在池中、照常跑定时
	// 任务（签到/猫猫旅行本身赚积分），充值后自动恢复。上游余额耗尽会发提醒短信，
	// 设阈值即避免被用到 0。
	reserveCredits int64
	// reserveByRealm 按域覆盖 reserveCredits（issue #147；键 "cn"/"global"）。
	// nil/空表 = 全部回落全局阈值。
	reserveByRealm map[string]int64
	// accountPriority 账号优先级（uid → 正整数，小 = 先消耗；SetAccountPriority 注入）。
	// issue #62：配了的账号构成「先烧层」——层内有号可用时未配置的号一律不接单，
	// 等价于「先把这批号的余额烧完，再换下一批」。硬过滤而非排序：排序只改抽签次序，
	// 做不到「优先消耗某个账号的余额」。缺省 nil = 全部未配置（选号行为与旧版一致）。
	accountPriority map[string]int
	// accountShare 账号占比（uid → 相对权重，正数；SetAccountShare 注入）。
	// issue #62：同一层内配了占比的账号独占抽签，按 share 归一化分流量（70/30 与
	// 7/3 同义，不必凑满 100）。缺省 nil = 全部未配置（行为与旧版一致）。
	accountShare map[string]float64
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// pickSeq 单调递增的选号序号：每次 pick 选中账号时自增并记到 entry.usedSeq，
	// 为 LRU 兜底/防惊群提供与 time.Now() 精度无关的严格全序（Windows ~0.5ms 精度下
	// lastUsed 墙钟会全等）。仅 pick 写锁路径读写，无需 atomic。
	pickSeq uint64
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	closeOnce sync.Once

	// strategy 选号策略（weighted / round_robin / sequential，见 strategy.go）。
	// 空 = weighted（既有行为）。它只决定「同样合格的候选里选哪个」——健康、冷却、
	// 在途、积分保底、成本分层、账号优先级全部先于它生效，策略无法绕过它们。
	strategy string
	// scheduling 策略游标：(realm, 模型) → 上次用到的账号。运行态不持久化
	// （重启从环头重来，只影响下一次的起点，不影响正确性）。
	scheduling map[string]scheduleCursor
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
		preferExpiring:     true,
		balanceWindow:      defaultBalanceWindow,
		degradeThreshold:   defaultDegradeThreshold,
		degradeCooldown:    defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
		// 探索缺省 30m：tier 0 垄断下的 tier 1 探索窗口（issue #136）。用户经
		// config 显式 "0" 关停（SetCostExploreInterval(0)）。
		costExploreInterval: defaultCostExploreInterval,
		exploreLast:         map[string]time.Time{},
		strategy:            StrategyWeighted,
		scheduling:          map[string]scheduleCursor{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// Close 停止后台落盘 goroutine 并做最后一次落盘（幂等）。
// 进程退出前调用，消除 startFlusher 的 goroutine 泄漏；不调用也不影响正确性
// （进程退出即回收），仅是生命周期卫生。
func (p *Pool) Close() {
	if p.stopCh == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stopCh)
	})
	p.Flush()
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetCostExploreInterval 注入 costTier 条件探索窗口（main 从 config 解析后调用，
// issue #136）。0 = 关停（完全回到现状行为）；正值覆盖默认 30m。
// 注意：与 SetSoftRateMax「非正值保留默认」不同，0 在这里是**合法值**（关停开关，
// 与 config 的 "0" 关停语义对齐）——不设 0 语义就无法关停探索。
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d < 0 {
		return // 负值非法，保留现值
	}
	p.costExploreInterval = d
}

// CostExploreStatus 透出探索台账（/status 用）：累计探索事件数 + 各 (域, 模型)
// 的最近探索时刻（键内 \x1f 分隔符输出为 "|"，与 model_costs 行对照即可读出
// 「探索→毕业」全链路）。RLock 只读遍历；map 大小受「服务过的 (域, 模型)」集合
// 约束（与 modelCost 同界，天然有界）。
func (p *Pool) CostExploreStatus() (events int64, last map[string]time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	last = make(map[string]time.Time, len(p.exploreLast))
	for k, ts := range p.exploreLast {
		// 键 realm+"\x1f"+model → 输出 "|"（JSON 安全可读；\x1f 不可打印）。
		last[strings.ReplaceAll(k, "\x1f", "|")] = ts
	}
	return p.costExploreEvents, last
}

// SetWeights 注入加权路由的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetReserveCredits 注入「保留积分」阈值（main 从 config 注入；0 = 关闭）。
// 与 SetCostExploreInterval 同风格：0 是合法值（关闭默认），负值非法保留现值。
// 不落 state.json——config 是事实源，重启由 main 重新注入。
func (p *Pool) SetReserveCredits(v int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v < 0 {
		return
	}
	p.reserveCredits = v
}

// SetReserveCreditsByRealm 注入按域覆盖表（issue #147；键 "cn"/"global"）。
// nil/空表 = 全部回落全局阈值。config 校验已剔非法键与负值，这里不再防御。
func (p *Pool) SetReserveCreditsByRealm(m map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserveByRealm = m
}

// reserveThreshold 该域的保留积分阈值：按域覆盖优先，未写回落全局值。
// 调用方须持锁；realm 为空当 "cn"（与 Auth.Realm 的归一化口径一致）。
func (p *Pool) reserveThreshold(realm string) int64 {
	if realm == "" {
		realm = "cn"
	}
	if v, ok := p.reserveByRealm[realm]; ok {
		return v
	}
	return p.reserveCredits
}

// SetPreferExpiring 注入最早到期优先路由开关与快过期窗口（main 从 config 解析后调用）。
// window <= 0 = 未注入 → 紧迫度退回旧的固定倍率（不引入梯度），便于嵌入方/单测保持旧行为。
func (p *Pool) SetPreferExpiring(enabled bool, window time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preferExpiring = enabled
	p.expiringWindow = window
}

// SetBalanceWindow 注入窗口用量调度窗口（issue #112）；d<=0 = 关闭。
// 关掉后不再记账（NoteModelCost 跳过）也不再降权，与未引入本功能的旧版行为一致。
func (p *Pool) SetBalanceWindow(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.balanceWindow = d
}

// SetAccountPriority 注入账号优先级表（uid → 正整数，小 = 先消耗；issue #62）。
// nil/空表 = 全部未配置，选号行为与未开此功能完全一致。非正值由 config 校验剔除。
// 与 SetReserveCredits 同风格：不落 state.json——config 是事实源，重启由 main 重新注入。
func (p *Pool) SetAccountPriority(m map[string]int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accountPriority = m
	// 静默失败是这类配置最坑的形态：uid 打错/字段名写错都不报错，只是"没生效"。
	// 打一行键数，配合逐号状态里的 priority 字段（面板 /panel/api/accounts）即可
	// 自证配置被读到且被匹配上。
	if len(m) > 0 {
		log.Printf("[pool] account_priority 已配置 %d 个账号（未配置的号在其后消费）", len(m))
	}
}

// SetAccountShare 注入账号占比表（uid → 相对权重，正数；issue #62）。
// nil/空表 = 全部未配置。占比在同一层内生效，先于「最早到期优先」（见 pick）。
func (p *Pool) SetAccountShare(m map[string]float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accountShare = m
	if len(m) > 0 {
		log.Printf("[pool] account_share 已配置 %d 个账号（同层内按归一化占比分流）", len(m))
	}
}

// SetDegrade 注入连败降权参数（main 从 config 解析后调用，issue #114）。
// 非正值保留原值（用默认，见 defaultDegrade*），风格同 SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// CreditFloor 透出生效的积分保底值（/status 用）。0 = 关闭。
func (p *Pool) CreditFloor() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.creditFloor
}

// SetCreditFloor 注入积分保底线（main 从 config 解析后调用）。
// 0 = 关闭（缺省即现状，零回归）；负值非法保留原值（0）。
// 语义见 Pool.creditFloor 字段注释。
func (p *Pool) SetCreditFloor(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.creditFloor = n
	}
}

// SetModelRateOf 注入上游目录积分倍率查表（main 用 upstream.Client.ModelRate 装配）。
// 供积分保底兜底判定「未实测过的模型是否收费」——本地台账无观测时，不能因为
// 「没学过」就无条件放行，否则高价新模型会把触底号一次性打穿（kimi-k3-1 实案：
// 全池无观测 → 保底全部放行 → 两笔扣 111 分打穿到 0 并硬冷却到次日 04:00）。
// fn 可为 nil（清注入）；回调只在持 p.mu 时被调用，不得反向调用 Pool 方法。
func (p *Pool) SetModelRateOf(fn func(realm, model string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modelRateOf = fn
}

// SetCreditFloorMaxRate 注入保底兜底允许的目录倍率上限（config pool.credit_floor_max_rate）。
//
// 解决两头堵：全额拦（旧行为）会让便宜的新模型在触底号上永远跑不出第一笔账本；
// 全额放（PR #2）会把 kimi-k3-1 那类高价模型的事故防护一起删掉。按倍率分档后
// 便宜模型（如 x0.11）能入账，贵的（x1.62）仍拦。0 = 严格模式（等于旧行为）。
// 负值非法保留原值。
func (p *Pool) SetCreditFloorMaxRate(v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v >= 0 {
		p.creditFloorMaxRate = v
	}
}

// CreditFloorMaxRate 透出生效的倍率上限（面板/status 用）。0 = 严格模式。
func (p *Pool) CreditFloorMaxRate() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creditFloorMaxRate
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal 注入 global 域单账号在途上限（WAF 403 修复 P1-1 分档）；
// 0 = 未设置，global 账号回落 maxInFlight（不分档）。负值保留原值。
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// inFlightLimit 报告账号的生效在途上限（global 分档优先，回落 maxInFlight）；
// 0 = 不限。调用方需已持 p.mu（或快照过 limit，见 Acquire）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
// Acquire 为 uid 占一个在途名额（会话粘性命中后调用）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。上限按账号 realm 分档
// （global 档 maxInFlightGlobal，P1-1；未设置回落 maxInFlight）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimit(e)
	p.mu.RUnlock()
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// AuthFor 返回账号凭证指针（只读用途：发起内部请求，如额度窗口探针）。
//
// 为什么需要它：池对外只给 Status（纯展示数据），而「用某个具体账号发一个最小请求」
// 需要 *auth.Auth 本体。返回指针而非拷贝是刻意的——refresh 会就地改写 token，拷贝
// 会让调用方拿着过期凭证打上游。
//
// 调用方**不得**修改返回值的可导出字段（除 auth 自带的加锁方法外）；uid 不存在返回 nil。
func (p *Pool) AuthFor(uid string) *auth.Auth {
	if uid == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed && !p.saveLocked() {
		p.dirty.Store(true) // 剔除结果没落盘：交给 flusher 重试，否则已删账号下次启动复活
	}
}

// InFlight 返回某账号当前在途请求数（不存在/已删除返回 0）。
//
// 热重载剔除账号前用它做一次「真的没人用」检查：把在途请求的账号从池里拿掉，
// 那条请求收尾时 Release 会对已删条目变成 no-op，计费/日志都还算得上，但面板上
// 会出现「账号消失时还有流量」。宁可下一轮扫描再删。
func (p *Pool) InFlight(uid string) int64 {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return 0
	}
	return e.inFlight.Load()
}

// RemoveMany 批量剔除账号并只落盘一次；返回实际剔除的 uid。热重载用。
//
// 与逐个 Remove 的差别：一次 saveLocked，避免 N 个账号批量消失时写 N 次 state.json。
// 不在池里的 uid 静默忽略（并发下另一个调用可能已经删过）。
func (p *Pool) RemoveMany(uids []string) []string {
	if len(uids) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var removed []string
	for _, uid := range uids {
		if _, ok := p.byUID[uid]; !ok {
			continue
		}
		delete(p.byUID, uid)
		removed = append(removed, uid)
	}
	if len(removed) > 0 && !p.saveLocked() {
		p.dirty.Store(true)
	}
	return removed
}

// Remove 从池中移除账号并立即落盘（管理面板用）。返回被移除账号的凭证
// （含 FilePath，供调用方删除 auth 文件）；uid 不存在返回 nil。
// 在途请求的 Release 对已删条目是 no-op，无需等待。
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	delete(p.byUID, uid)
	p.dirty.Store(true)
	p.saveLocked()
	return e.a
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
