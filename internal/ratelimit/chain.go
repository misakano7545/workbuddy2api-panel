package ratelimit

import (
	"sort"
	"strings"
)

// Step 是降级链上的一步：按数组顺序逐级尝试——前一个也限流了才试下一个。
type Step struct {
	Model     string  `json:"model"`
	Rate      string  `json:"rate,omitempty"`
	RateValue float64 `json:"rate_value"`
	HasRate   bool    `json:"has_rate"`
	ZeroRate  bool    `json:"zero_rate"`
	// Reason 说明这一步是怎么进链的（人工 / fallback 表 / 自动挑的倍率），
	// 页面直接展示：运营看到「自动：零倍率」才知道这不是自己配的。
	Reason string `json:"reason,omitempty"`
}

// 降级链来源标签。页面与导出包都用它们说明「这条链是谁定的」。
const (
	ChainSourceManual    = "manual"
	ChainSourceFallback  = "fallback"
	ChainSourceAutoZero  = "auto-zero"
	ChainSourceAutoCheap = "auto-cheap"
	ChainSourceNone      = "none"
)

// 一条链最多几级：再多就是「反正都会失败」，页面也展示不下。
const maxChainSteps = 4

// Chain 解析 (域, 模型) 的降级链及其来源。
//
// 优先级（人工意图永远压过自动推断）：
//  1. 策略里写死的 Chain：运维明确指定，必须尊重；
//  2. 池的 pool.model_fallback 表：老配置里的等价模型，页面不重复维护；
//  3. ChainMode=zero/auto 的自动链：零倍率（免费额度型）优先，其次倍率最低。
//     ChainMode=off/fallback 时不做自动推断——「没配过却被自动改道」是最难
//     排查的一类事故，自动链必须在策略里显式打开。
//
// 返回的 source 是 ChainSource* 之一；无链时返回 ChainSourceNone。
func Chain(l ModelLimit, realm, model string, rates map[string]string, candidates []string, fallback map[string]string) ([]Step, string) {
	if steps := manualChain(l, realm, model, rates); len(steps) > 0 {
		return steps, ChainSourceManual
	}
	if steps := fallbackChain(l, realm, model, rates, fallback); len(steps) > 0 {
		return steps, ChainSourceFallback
	}
	switch l.ChainMode {
	case ChainZero:
		if steps := autoChain(realm, model, rates, candidates, true); len(steps) > 0 {
			return steps, ChainSourceAutoZero
		}
	case ChainAuto:
		steps := autoChain(realm, model, rates, candidates, false)
		if len(steps) > 0 {
			if steps[0].ZeroRate {
				return steps, ChainSourceAutoZero
			}
			return steps, ChainSourceAutoCheap
		}
	}
	return nil, ChainSourceNone
}

func manualChain(l ModelLimit, realm, model string, rates map[string]string) []Step {
	if len(l.Chain) == 0 {
		return nil
	}
	out := make([]Step, 0, len(l.Chain))
	for _, name := range l.Chain {
		if sameModel(name, model) {
			continue // 自己不能当自己的降级目标，否则 6004 会原地打转
		}
		out = append(out, makeStep(name, realm, rates, "手动指定"))
		if len(out) >= maxChainSteps {
			break
		}
	}
	return out
}

// fallbackChain 读 pool.model_fallback 表。注意 ChainMode=off 依然认这张表：
// 那是运维早就配好的等价关系，「关掉自动链」只该关掉推断，不该顺手废掉已有配置。
func fallbackChain(l ModelLimit, realm, model string, rates map[string]string, fallback map[string]string) []Step {
	_ = l
	if len(fallback) == 0 {
		return nil
	}
	target := ""
	if v, ok := fallback[realm+":"+model]; ok {
		target = v
	} else if v, ok := fallback[model]; ok {
		target = v
	}
	target = strings.TrimSpace(target)
	if target == "" || sameModel(target, model) {
		return nil
	}
	return []Step{makeStep(target, realm, rates, "pool.model_fallback 表")}
}

// autoChain 按倍率自动推链。zeroOnly 为 true 时只收倍率 0 的模型（省钱预设的
// 语义就是「绝不换到更贵的档」，宁可没有链）。
func autoChain(realm, model string, rates map[string]string, candidates []string, zeroOnly bool) []Step {
	pool := map[string]bool{}
	for _, name := range candidates {
		pool[strings.TrimSpace(name)] = true
	}
	for name := range rates {
		pool[strings.TrimSpace(name)] = true
	}
	type cand struct {
		name string
		rate float64
	}
	var zeros, others []cand
	for name := range pool {
		if name == "" || sameModel(name, model) {
			continue
		}
		v := ParseRate(rates[bareFor(name, realm)])
		// 未知倍率的模型不进自动链：无法比较价格就无法为「降到哪儿」负责。
		if v < 0 {
			continue
		}
		if v == 0 {
			zeros = append(zeros, cand{name, v})
			continue
		}
		if !zeroOnly {
			others = append(others, cand{name, v})
		}
	}
	sort.Slice(zeros, func(i, j int) bool { return zeros[i].name < zeros[j].name })
	sort.Slice(others, func(i, j int) bool {
		if others[i].rate != others[j].rate {
			return others[i].rate < others[j].rate
		}
		return others[i].name < others[j].name
	})
	// 自动链只留一个「付费兜底」，避免把一次请求在多个收费档之间来回搬运
	// （每搬一次都是一次真实的积分支出）。所以零倍率最多占 maxChainSteps-1 个坑：
	// 否则 4 个免费档正好填满链长时付费档永远进不来（而目标模型自己若是零倍率，
	// 会少一个候选，反而有兜底——表现成「只有某个模型没有兜底」）。
	zeroCap := maxChainSteps
	if !zeroOnly && len(others) > 0 {
		zeroCap--
	}
	out := make([]Step, 0, maxChainSteps)
	for _, c := range zeros {
		if len(out) >= zeroCap {
			break
		}
		out = append(out, makeStep(c.name, realm, rates, "自动：零倍率免费档"))
	}
	if !zeroOnly && len(others) > 0 {
		out = append(out, makeStep(others[0].name, realm, rates, "自动：最低倍率"))
	}
	return out
}

func makeStep(name, realm string, rates map[string]string, reason string) Step {
	name = strings.TrimSpace(name)
	bare := bareFor(name, realm)
	rate := rates[bare]
	v := ParseRate(rate)
	return Step{Model: name, Rate: rate, RateValue: v, HasRate: v >= 0, ZeroRate: v == 0, Reason: reason}
}

// bareFor 剥掉与目标域一致的域前缀；跨域目标保留全名（页面据此提示跨域降级）。
func bareFor(name, realm string) string {
	if r, bare, ok := splitModel(name); ok && (realm == "" || strings.EqualFold(r, realm)) {
		return bare
	}
	return name
}

// sameModel 判断两个模型名是否指向同一个模型（忽略域前缀与大小写）。
func sameModel(a, b string) bool {
	return strings.EqualFold(bareName(a), bareName(b))
}

func bareName(name string) string {
	name = strings.TrimSpace(name)
	if _, bare, ok := splitModel(name); ok {
		name = bare
	}
	return name
}
