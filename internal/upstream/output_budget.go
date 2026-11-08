// output_budget.go 输出预算下限：客户端挡不住「高档位思考把自己那份额度吃光」的
// 组合错误，网关兜住它。
//
// 现象（2026-11-08 生产实测，cn:deepseek-v4.1-flash，要求写 3000+ 字长文档）：
//
//	effort=high + max_tokens=2048 → total=4096 think=2048 正文=2048 finish=length
//	effort=max  + max_tokens=2048 → total=4096 think=2048 正文=2048 finish=length
//	effort=high + 不给额度        → total=16720 think=2998 正文=13722 finish=stop
//
// 即：思考与正文在**同一份额度**里结算时，客户端给的 2048 先被思考吃掉一半，
// 正文只剩 1024 个 token 就断在句子中间——用户看到的就是「回答写到一半停住」。
// 客户端并不知道模型的思考要花多少，它只能给一个拍脑袋的数，这个组合错误只能由
// 网关修：把 max_tokens 抬到「这一档思考起码要花的量」以上。
//
// 抬的是**上限**不是目标长度：模型写完自然停（实测 high+大额度收尾在 13722 token，
// 不会因为上限 8192/16384 而多写一个字）。所以这既不改变答案长度，也不额外计费，
// 只是把「预算被思考偷走」这个 bug 修掉。
//
// 与 starved.go / continue.go 的分工：
//   - 这里在**出站前**兜住（最彻底，客户端根本看不到截断）；
//   - starved.go 兜「额度仍被吃光、正文 0B」的残留形态（关思考重发一次）；
//   - continue.go 兜「共用额度、思考占大头但已写出正文」的残留形态（关思考续写）。
// 三者按「越早越省」排列，后者只是前者的保险。
package upstream

import (
	"log"
	"strings"
)

// minBudgetByEffort 各思考档位下「正文还能写得出来」的最小输出预算。
//
// 取值依据（生产实测 think 用量，同一模型 cn:deepseek-v4.1-flash）：
//
//	low   → think 1853~2722
//	high  → think 2048~4068
//	max   → think 7612~37110（同一个提示词能从 7.6k 波动到 37k）
//
// 下限按「该档思考的上界」再留一倍给正文，所以是 4k/8k/16k：低于它，思考必然
// 先把预算吃掉一半以上；高于它，思考与正文都够用，模型自然收尾。
var minBudgetByEffort = map[string]int64{
	"minimal": 4096,
	"low":     4096,
	"medium":  6144,
	"high":    8192,
	"xhigh":   12288,
	"max":     16384,
}

// minOutputBudgetProbe 低于这个额度的请求一律不动：官方客户端用 max_tokens=1 做
// key 探活（"Reply with OK."），抬上去没有意义，还会让探活变慢。与 starved.go 的
// minStarvedBudget 同口径。
const minOutputBudgetProbe = 16

// MinOutputBudget 该思考档位下「正文还能写得出来」的最小输出预算；档位未知返回 0。
//
// 供续写/救援段复用：那些段是从**客户端原始 body** 重建的（不含首段被抬过的
// max_tokens），而它们往往关掉思考，prepareBody 的下限逻辑会跳过（没有思考就不
// 存在「思考吃光预算」）。若不显式带上，救援段会继续用客户端那个装不下正文的小额度
// ——线上实测正是这样：首段 8192 全被思考吃掉 → 救援段只拿到 1024 → 正文 1946 字
// 就断。所以续写层必须自己把下限写进去。
func MinOutputBudget(effort string) int64 {
	return minBudgetByEffort[strings.ToLower(strings.TrimSpace(effort))]
}

// EnsureMinBudget 把 obj 的 max_tokens 抬到不低于 floor（floor<=0 或当前额度是探活级
// 时不动）。返回是否改动。
func EnsureMinBudget(obj map[string]any, floor int64) bool {
	if floor <= 0 {
		return false
	}
	v, ok := explicitOutputBudget(obj)
	if !ok || v <= 0 || v < minOutputBudgetProbe || v >= floor {
		return false
	}
	obj["max_tokens"] = floor
	return true
}

// ensureMinOutputBudget 在「客户端显式给了 max_tokens」且「低于该档下限」时抬到下限。
//
// 不动的四种情形（都不猜）：
//   - 没有显式额度：injectCatalogMaxTokens 已按目录补了真实上限，无需再动；
//   - 额度是探活级（< 16）；
//   - 档位为空或关闭思考（thinking.type=disabled / effort=off）：没有思考要花钱；
//   - 已经不低于下限。
//
// 目录 maxOutputTokens 已知且更小时以目录为准——不把额度抬到模型物理上写不了的值。
func ensureMinOutputBudget(obj map[string]any, effort string, maxOut map[string]int64) {
	v, ok := explicitOutputBudget(obj)
	if !ok || v <= 0 || v < minOutputBudgetProbe {
		return
	}
	floor, has := minBudgetByEffort[strings.ToLower(strings.TrimSpace(effort))]
	if !has {
		return
	}
	if thinkingDisabled(obj) {
		return
	}
	if model, _ := obj["model"].(string); len(maxOut) > 0 {
		if cap, ok := maxOut[model]; ok && cap > 0 && floor > cap {
			floor = cap
		}
	}
	if v >= floor {
		return
	}
	obj["max_tokens"] = floor
	log.Printf("INFO: [budget] max_tokens %d -> %d（effort=%s 的输出预算下限：低于它时思考会先吃掉一半以上，正文被截断在句子中间；抬的是上限不是目标长度）",
		v, floor, effort)
}

// explicitOutputBudget 读客户端显式给的输出额度（三个别名，translateMaxCompletionTokens
// 已把别名折进 max_tokens，这里仍按三个键判「有没有显式给」）。
func explicitOutputBudget(obj map[string]any) (int64, bool) {
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		switch v := obj[k].(type) {
		case float64:
			return int64(v), true
		case int:
			return int64(v), true
		case int64:
			return v, true
		case string:
			// 个别客户端把额度写成字符串；解析失败按「没给」处理，交给目录兜底。
			if n := parseInt64(v); n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}
