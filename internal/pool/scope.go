// scope.go 号池分区：文本号池 / 媒体号池（借鉴 ardeyouxipianyi/workbuddy2api-hub 的
// wb_pool_scope.py）。
//
// 为什么需要：文本对话与图片/视频生成在上游是两套风控口径。同一个号既跑对话又画图时，
// 画图侧被判异常会连坐文本侧——而文本侧是日常主力，一次连坐的代价远高于少一个画图号。
// 分区后两类请求各自从自己的号池里选，互不污染。
//
// 与参照实现的差异（**必须知道**）：
//
//	hub 默认关闭分区，我们的分区也是默认关闭（pool.separate_text_media）。
//	hub 的隔离是把「文本派发」与「媒体派发」做成两个模块各持一份策略；本仓的选号
//	只有一个入口（Pool.pick），所以分区做成**候选过滤谓词**：一条路径决定这次请求
//	属于哪个池，pick 按池过滤。好处是不存在「两份策略漂移」的可能，坏处是任何绕过
//	pick 的路径（粘性号、面板钉号、API Key 绑定号）都必须显式再校验一次——那是
//	CheckScope 存在的唯一理由。
//
// 缺省值：账号未标注时归 **text**。这是刻意的保守选择——新导入的号默认按文本号
// 使用（与分区前的行为一致），要拿它画图必须由运维显式标成 media。反过来（缺省
// media）会让所有老部署的画图请求在新版本第一天全部 403。
package pool

import (
	"errors"
	"fmt"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

const (
	// ScopeText 文本号池：对话 / Responses / embeddings 等文本类请求。
	ScopeText = "text"
	// ScopeMedia 媒体号池：图片 / 视频生成。
	ScopeMedia = "media"
)

// NormalizeScope 归一化号池标签：空值（未标注）按文本号池处理。
func NormalizeScope(scope string) string {
	if scope == ScopeMedia {
		return ScopeMedia
	}
	return ScopeText
}

// ScopeViolation 账号与请求所属号池不匹配。与上游错误分开：这不是请求失败，
// 是网关侧的**硬边界**——调用方不该重试，而应该去改账号标注或改用另一个号。
type ScopeViolation struct {
	UID    string
	Scope  string // 账号实际所属
	Wanted string // 本次请求需要的
	Msg    string
}

func (e *ScopeViolation) Error() string { return e.Msg }

// ScopeLabel 号池的中文名（面板与报错文案共用，避免两处各写一套）。
func ScopeLabel(scope string) string {
	if NormalizeScope(scope) == ScopeMedia {
		return "媒体号池"
	}
	return "文本号池"
}

// SetScopeEnforced 打开/关闭分区强制（pool.separate_text_media）。
//
// 关闭时 pick 不做过滤、CheckScope 恒放行——分区前的行为逐字不变（零回归）。
// 打开后已标注的账号只在各自池内被选；未标注的按 text 处理。
func (p *Pool) SetScopeEnforced(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scopeEnforced == on {
		return
	}
	p.scopeEnforced = on
	p.dirty.Store(true)
}

// ScopeEnforced 报告分区是否在强制。
func (p *Pool) ScopeEnforced() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.scopeEnforced
}

// SetScope 设置账号的号池标签（ScopeText / ScopeMedia；空串 = 清除标注，回落 text）。
// 返回 false 表示账号不存在（供调用方区分「改成功了」与「uid 打错」）。
func (p *Pool) SetScope(uid, scope string) bool {
	scope = NormalizeScope(scope)
	if scope == ScopeText {
		// 缺省值不落盘：state.json 里少一半噪音，语义等价（读侧同样归一）。
		scope = ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if e.scope == scope {
		return true
	}
	e.scope = scope
	p.dirty.Store(true)
	return true
}

// ScopeOf 账号的号池标签（未标注 → ScopeText）。
func (p *Pool) ScopeOf(uid string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return ""
	}
	return NormalizeScope(e.scope)
}

// allowsScopeLocked 判定一条候选是否属于目标号池。调用方须持锁（RLock 即可）。
func (p *Pool) allowsScopeLocked(e *entry, scope string) bool {
	if !p.scopeEnforced || scope == "" {
		return true
	}
	return NormalizeScope(e.scope) == scope
}

// CheckScope 校验一个**已经拿在手上**的账号能不能承接目标号池的请求。
//
// 为什么必须有这条：pick 之外还有几条路径会直接把账号塞进请求——会话粘性
// （PickByUIDForModel）、面板钉号、API Key 绑定号、重试链沿用的上一轮账号。
// 只在 pick 里过滤等于给媒体号留了后门（参照实现的原话：「这些路径都不过
// pick()，只靠选号过滤等于给媒体账号留了一条后门」）。
//
// 分区关闭时恒放行；账号不存在时返回 nil（由调用方各自的 404 处理）。
func (p *Pool) CheckScope(a *auth.Auth, scope string) error {
	if a == nil || scope == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.scopeEnforced {
		return nil
	}
	e, ok := p.byUID[a.UID]
	if !ok {
		return nil
	}
	have := NormalizeScope(e.scope)
	if have == scope {
		return nil
	}
	return &ScopeViolation{
		UID:    a.UID,
		Scope:  have,
		Wanted: scope,
		Msg: fmt.Sprintf("账号 %s 属于%s，不能承接%s的请求（pool.separate_text_media 已开启）；"+
			"请改用对应号池的账号，或先在看板改这个账号的号池标注",
			scopeShort(a.UID), ScopeLabel(have), ScopeLabel(scope)),
	}
}

// IsScopeViolation 判断错误是否为号池越界（调用方据此决定换号还是直接 403）。
func IsScopeViolation(err error) bool {
	var v *ScopeViolation
	return errors.As(err, &v)
}

// scopeShort 报错文案里的账号称呼：前 8 位（同 logfmt.Label 的口径，不落完整 UID）。
func scopeShort(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	if uid == "" {
		return "?"
	}
	return uid
}
