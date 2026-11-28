// Package ratelimit 模型限流映射：额度口径、降级链与策略持久化。
//
// 三处消费方共用同一段代码，这是本功能能成立的前提：
//   - internal/panel 的「模型限流映射」页（读快照、改策略、导出提交包）
//   - internal/server 请求路径（选号前拦/改道，上游 6004 时逐级降级）
//   - 导出的策略包（export 生成，供上游仓库对齐同一套口径）
//
// 自包含是第一约束：本包不 import 仓库内其它 internal 包，外部数据（用量桶、
// 账号快照、模型倍率、池的降级表）全部经 Deps 闭包注入。因此测试不需要起池子
// 或记录器；「为提交上游做准备」时也只需带走这一个目录 + 两个调用点（面板路由、
// handler 选号前），不会把 fork 的本地增量一并拖进去。
package ratelimit

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 口径来源。
const (
	// SourceFollow 跟随上游网关推算额度（默认）：配额取上游已知的「每账号每模型
	// 窗口 token 上限」，本地只观测、不额外设预算；真正撞顶由上游 6004 反馈。
	SourceFollow = "follow"
	// SourceManual 手动设限：窗口、配额、命中动作都由策略指定；配合 OnHit 可以在
	// 上游还没反馈之前就本地改道或停派。
	SourceManual = "manual"
)

// 命中动作（额度耗尽时怎么办）。
const (
	OnHitDegrade     = "degrade"     // 改道降级链首（默认）
	OnHitHold        = "hold"        // 停派该模型：请求直接 429，不悄悄换模型
	OnHitPassthrough = "passthrough" // 只观测记账，不拦不改道
)

// 降级链来源口径。
const (
	ChainOff      = "off"      // 不用自动链：只认策略写死的 Chain 与池的 model_fallback 表
	ChainFallback = "fallback" // 只认池的 model_fallback 表
	ChainZero     = "zero"     // 自动链只收「积分倍率 0」的模型（免费额度型，如 hy4flash-free）
	ChainAuto     = "auto"     // 自动链：零倍率优先，其次倍率最低（默认）
)

// DefaultWindowSec 默认统计窗口 24h 滚动，与上游「每账号每模型 24h 配额」同口径
// （不是自然日重置：从该账号该模型窗口内第一次请求起算）。
const DefaultWindowSec int64 = 86400

// 窗口合法区间：太短会让阈值随单次大请求抖动，太长则「限流」失去意义。
const (
	minWindowSec int64 = 60
	maxWindowSec int64 = 30 * 86400
)

// followTokenQuota 是「跟随上游」口径下每账号每模型每窗口的 token 配额。
//
// 数值来源：上游 CN 域每账号每模型 200M token、global 域 100M，滚动 24h；与
// cmd/credit-forecast 的 defaultQuotas、.atomcode/memory.md 记录的实测口径同源。
// 这是推算值而非上游合同值：页面必须把推断出来的行标成 inferred，否则运营会拿
// 它当承诺，在临界点上误判「还有额度」。
var followTokenQuota = map[string]int64{"cn": 200_000_000, "global": 100_000_000}

// FollowTokenQuota 返回某域「跟随上游」的每账号每模型窗口 token 配额。
// 未知域按 CN 口径兜底（保守：宁可高估额度也别在别的域误拦）。
func FollowTokenQuota(realm string) int64 {
	if q, ok := followTokenQuota[normalizeRealm(realm)]; ok {
		return q
	}
	return followTokenQuota["cn"]
}

// ModelLimit 是一条限流策略。作用域由 Realm × Model 决定，越具体越优先：
// 默认（两者皆空）< 域级 < 模型级 < 域+模型。
//
// 零值的语义（由 ResolveQuota 展开，页面据此决定显示「推算」还是「不限」）：
//   - TokenQuotaPerAccount == 0 → 沿用上游推算值（followTokenQuota）
//   - CreditQuotaPerAccount / CreditQuotaPerPool == 0 → 该维度不设限
type ModelLimit struct {
	Realm string `json:"realm,omitempty"` // cn | global | ""（两域）
	Model string `json:"model,omitempty"` // 裸模型名；"" = 该域全部模型

	Source string `json:"source,omitempty"` // follow | manual

	WindowSec int64 `json:"window_sec,omitempty"`

	// 额度：token 与积分两种维度可以同时设，命中任一即触发 OnHit。
	TokenQuotaPerAccount  int64   `json:"token_quota_per_account,omitempty"`
	CreditQuotaPerAccount float64 `json:"credit_quota_per_account,omitempty"`
	CreditQuotaPerPool    float64 `json:"credit_quota_per_pool,omitempty"`

	OnHit string `json:"on_hit,omitempty"` // degrade | hold | passthrough

	// Chain 是人工指定的降级链（按顺序尝试）。非空时优先于 fallback 表与自动链。
	Chain []string `json:"chain,omitempty"`
	// ChainMode 决定没有人工链时怎么自动推：off | fallback | zero | auto。
	ChainMode string `json:"chain_mode,omitempty"`

	Note string `json:"note,omitempty"`
}

// Policy 是一整份限流策略：默认口径 + 若干覆盖项 + 当前预设名。
type Policy struct {
	Preset    string       `json:"preset,omitempty"`
	Default   ModelLimit   `json:"default"`
	Overrides []ModelLimit `json:"overrides,omitempty"`
	UpdatedAt time.Time    `json:"updated_at,omitempty"`
}

// Preset 是页面上一键套用的策略组合。
type Preset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"`
	// Default/Overrides 是套用后写进策略的内容（页面的「预览」直接读它）。
	Default   ModelLimit   `json:"default"`
	Overrides []ModelLimit `json:"overrides,omitempty"`
}

// Presets 返回内置预设。四个预设的差异全部落在「谁触发降级 + 落到哪儿」上，
// 而不是散在各处的一堆开关——运营只需要回答一个问题：预算用完后要不要继续服务。
func Presets() []Preset {
	return []Preset{
		{
			ID:   "follow",
			Name: "跟随上游",
			Note: "默认口径。额度按上游推算（CN 200M / global 100M 每账号每模型 24h 滚动），" +
				"本地不额外设预算；只在上游返回 6004 时按池的 model_fallback 表降级。",
			Default: ModelLimit{Source: SourceFollow, OnHit: OnHitDegrade, ChainMode: ChainFallback},
		},
		{
			ID:   "balanced",
			Name: "均衡",
			Note: "跟随上游额度，额外允许设池级积分预算；上游额度或预算吃紧时改道到自动链" +
				"（零倍率优先，其次最便宜），让请求不失败。",
			Default: ModelLimit{Source: SourceFollow, OnHit: OnHitDegrade, ChainMode: ChainAuto},
		},
		{
			ID:   "saver",
			Name: "省钱",
			Note: "主动把流量压到免费档：窗口配额收到上游额度的 1/4，" +
				"超限只落到积分倍率为 0 的模型（如 hy4flash-free），不换到更贵的付费档。",
			Default: ModelLimit{
				Source: SourceManual, OnHit: OnHitDegrade, ChainMode: ChainZero,
				TokenQuotaPerAccount: 50_000_000,
			},
			Overrides: []ModelLimit{{Realm: "global", TokenQuotaPerAccount: 25_000_000}},
		},
		{
			ID:   "quality",
			Name: "质量优先",
			Note: "按上游额度严格设限（与「跟随上游」同数），但撞顶直接 429 不降级：" +
				"宁可让调用方知道这个模型暂时不可用，也不悄悄换成低质模型。",
			Default: ModelLimit{Source: SourceManual, OnHit: OnHitHold, ChainMode: ChainOff},
		},
	}
}

// FindPreset 按 ID 找预设。
func FindPreset(id string) (Preset, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range Presets() {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// DefaultPolicy 是全新部署（还没有策略文件）时生效的策略：跟随上游。
func DefaultPolicy() Policy {
	p, _ := FindPreset("follow")
	return Policy{Preset: p.ID, Default: Normalize(p.Default)}
}

// ApplyPreset 用预设整体替换策略（Default + Overrides + Preset 名）。
//
// 为什么连 Overrides 一起替换：预设承诺的是「整套口径」，保留旧覆盖项会让页面
// 显示的口径与预设文案不一致，出问题时没人能按文案复述现场。误点只需再点一次
// 另一个预设，代价远小于口径不明的代价。
func ApplyPreset(p Policy, id string) (Policy, error) {
	preset, ok := FindPreset(id)
	if !ok {
		return p, fmt.Errorf("未知预设 %q", id)
	}
	out := Policy{Preset: preset.ID, Default: Normalize(preset.Default), UpdatedAt: p.UpdatedAt}
	for _, o := range preset.Overrides {
		out.Overrides = append(out.Overrides, Normalize(o))
	}
	if err := Validate(out); err != nil {
		return p, err
	}
	return out, nil
}

// Normalize 归一化单条策略：小写域、剥掉模型名上的域前缀、补齐枚举与窗口默认值。
//
// 它只负责「同一语义的不同写法产生同一结果」（upsert 判等、覆盖项合并、导出后
// 回填都靠它），不改用户写下的数值——0 的语义留给 ResolveQuota 展开。
func Normalize(l ModelLimit) ModelLimit {
	out := l
	out.Realm = normalizeRealm(out.Realm)
	out.Model = strings.TrimSpace(out.Model)
	if realm, bare, ok := splitModel(out.Model); ok {
		if out.Realm == "" {
			out.Realm = realm
		}
		out.Model = bare
	}
	out.Source = strings.ToLower(strings.TrimSpace(out.Source))
	if out.Source != SourceManual {
		out.Source = SourceFollow
	}
	out.OnHit = strings.ToLower(strings.TrimSpace(out.OnHit))
	switch out.OnHit {
	case OnHitHold, OnHitPassthrough, OnHitDegrade:
	default:
		out.OnHit = OnHitDegrade
	}
	out.ChainMode = strings.ToLower(strings.TrimSpace(out.ChainMode))
	switch out.ChainMode {
	case ChainOff, ChainFallback, ChainZero, ChainAuto:
	default:
		out.ChainMode = ChainAuto
	}
	if out.WindowSec <= 0 {
		out.WindowSec = DefaultWindowSec
	}
	out.Chain = normalizeChain(out.Chain)
	out.Note = strings.TrimSpace(out.Note)
	return out
}

// Validate 校验整份策略。策略写坏比没有这个功能更糟——请求路径会按错的额度拦
// 真实流量，所以落盘前一律过这一关。
func Validate(p Policy) error {
	if err := ValidateLimit(Normalize(p.Default)); err != nil {
		return fmt.Errorf("默认策略: %w", err)
	}
	seen := map[string]bool{}
	for i, o := range p.Overrides {
		n := Normalize(o)
		if err := ValidateLimit(n); err != nil {
			return fmt.Errorf("覆盖项 #%d: %w", i+1, err)
		}
		key := n.Realm + "/" + strings.ToLower(n.Model)
		if seen[key] {
			return fmt.Errorf("覆盖项 #%d 与前面的作用域重复：%s", i+1, scopeName(n.Realm, n.Model))
		}
		seen[key] = true
	}
	return nil
}

// ValidateLimit 校验单条策略。
func ValidateLimit(l ModelLimit) error {
	if l.Realm != "" && l.Realm != "cn" && l.Realm != "global" {
		return fmt.Errorf("未知域 %q（只支持 cn / global，或留空表示两域）", l.Realm)
	}
	if l.Model != "" && strings.ContainsAny(l.Model, "\t\r\n") {
		return errors.New("模型名不能含空白字符")
	}
	if l.WindowSec < minWindowSec || l.WindowSec > maxWindowSec {
		return fmt.Errorf("窗口 %d 秒超出允许区间 [%d, %d]", l.WindowSec, minWindowSec, maxWindowSec)
	}
	if l.TokenQuotaPerAccount < 0 {
		return errors.New("token 配额不能为负")
	}
	if l.CreditQuotaPerAccount < 0 || l.CreditQuotaPerPool < 0 {
		return errors.New("积分配额不能为负")
	}
	for _, c := range l.Chain {
		if strings.ContainsAny(c, "\t\r\n") {
			return fmt.Errorf("降级链里的模型名 %q 含空白字符", c)
		}
	}
	return nil
}

// EffectiveLimit 合并出 (域, 模型) 的生效策略：默认值起步，覆盖项按「越具体越晚
// 应用」的顺序叠上去，后写的非零字段覆盖先写的。
//
// 为什么按具体度排序而不是按数组顺序：运维调整覆盖项顺序不该改变生效口径，
// 否则「同一条覆盖，挪了个位置就变行为」会成为无法解释的灵异问题。
func EffectiveLimit(p Policy, realm, model string) ModelLimit {
	out := Normalize(p.Default)
	ordered := make([]ModelLimit, 0, len(p.Overrides))
	for _, o := range p.Overrides {
		ordered = append(ordered, Normalize(o))
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return specificity(ordered[i]) < specificity(ordered[j])
	})
	for _, o := range ordered {
		if !scopeMatch(o, realm, model) {
			continue
		}
		out = mergeLimit(out, o)
	}
	return out
}

// specificity 越大越具体：域级 1、模型级 2、域+模型 3、全局默认 0。
func specificity(l ModelLimit) int {
	s := 0
	if l.Realm != "" {
		s++
	}
	if l.Model != "" {
		s += 2
	}
	return s
}

func scopeMatch(l ModelLimit, realm, model string) bool {
	if l.Realm != "" && !strings.EqualFold(l.Realm, normalizeRealm(realm)) {
		return false
	}
	if l.Model != "" && !strings.EqualFold(l.Model, strings.TrimSpace(model)) {
		return false
	}
	return l.Realm != "" || l.Model != ""
}

// mergeLimit 把 over 的非零字段叠到 base 上。零值表示「继承」，因此不能用零值
// 显式清空某一维度——要「不限该维度」请把 Source 设成 follow。
func mergeLimit(base, over ModelLimit) ModelLimit {
	out := base
	if over.Source != "" {
		out.Source = over.Source
	}
	if over.WindowSec > 0 {
		out.WindowSec = over.WindowSec
	}
	if over.TokenQuotaPerAccount > 0 {
		out.TokenQuotaPerAccount = over.TokenQuotaPerAccount
	}
	if over.CreditQuotaPerAccount > 0 {
		out.CreditQuotaPerAccount = over.CreditQuotaPerAccount
	}
	if over.CreditQuotaPerPool > 0 {
		out.CreditQuotaPerPool = over.CreditQuotaPerPool
	}
	if over.OnHit != "" {
		out.OnHit = over.OnHit
	}
	if len(over.Chain) > 0 {
		out.Chain = over.Chain
	}
	if over.ChainMode != "" {
		out.ChainMode = over.ChainMode
	}
	if over.Note != "" {
		out.Note = over.Note
	}
	return out
}

// Quota 是某行展开后的额度口径（0 已被换成具体数字或明确的「不限」）。
type Quota struct {
	WindowSec        int64   `json:"window_sec"`
	TokenPerAccount  int64   `json:"token_per_account"`
	CreditPerAccount float64 `json:"credit_per_account"`
	TokenPerPool     int64   `json:"token_per_pool"`
	CreditPerPool    float64 `json:"credit_per_pool"`
	// Inferred 表示 token 配额不是策略写的，而是按上游口径推算的。
	Inferred bool `json:"inferred"`
}

// ResolveQuota 把策略里的 0 值展开成实际口径。routable 是参与选号的账号数，
// 池级 token 配额 = 单账号配额 × routable——上游配额本来就是按账号发的，
// 池的上限就是「还能用的账号数 × 每号额度」，与 cmd/credit-forecast 的池级
// 推算口径一致。
func ResolveQuota(l ModelLimit, realm string, routable int) Quota {
	q := Quota{WindowSec: l.WindowSec, CreditPerAccount: l.CreditQuotaPerAccount, CreditPerPool: l.CreditQuotaPerPool}
	if q.WindowSec <= 0 {
		q.WindowSec = DefaultWindowSec
	}
	if l.TokenQuotaPerAccount > 0 {
		q.TokenPerAccount = l.TokenQuotaPerAccount
	} else {
		q.TokenPerAccount = FollowTokenQuota(realm)
		q.Inferred = true
	}
	if routable < 0 {
		routable = 0
	}
	q.TokenPerPool = q.TokenPerAccount * int64(routable)
	if q.CreditPerPool <= 0 && q.CreditPerAccount > 0 {
		q.CreditPerPool = q.CreditPerAccount * float64(routable)
	}
	return q
}

// ParseRate 把上游倍率字符串转成数值；空串/非数值返回 -1（未知），0 是合法值
// （免费模型）。把未知当「免费」是刻意的禁区：那会让自动链把流量全导到收费模型上。
func ParseRate(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	s = strings.TrimSuffix(strings.ToLower(s), "x")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return v
}

func normalizeRealm(realm string) string {
	return strings.ToLower(strings.TrimSpace(realm))
}

// splitModel 拆掉模型名上的域前缀（"cn:gpt-5.4" → realm=cn, bare=gpt-5.4）。
func splitModel(model string) (realm, bare string, ok bool) {
	i := strings.Index(model, ":")
	if i <= 0 || i == len(model)-1 {
		return "", model, false
	}
	prefix := normalizeRealm(model[:i])
	if prefix != "cn" && prefix != "global" {
		return "", model, false
	}
	return prefix, model[i+1:], true
}

func normalizeChain(chain []string) []string {
	if len(chain) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(chain))
	for _, c := range chain {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		key := strings.ToLower(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scopeName 是给人看的作用域名（日志、报错、导出表里统一用它）。
func scopeName(realm, model string) string {
	switch {
	case realm != "" && model != "":
		return realm + ":" + model
	case model != "":
		return model + "（两域）"
	case realm != "":
		return realm + "（全域）"
	}
	return "全局默认"
}
