package server

import (
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 规则实现在 upstream.SplitRealmPrefix：出站 body 也在同一次解析里剥前缀，
// 两处必须同口径，故委托而非各写一份。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	return upstream.SplitRealmPrefix(model)
}

// attemptModel 返回一次上游尝试**实际使用**的模型名（裸名，已剥域前缀）。
//
// 为什么要三个入参：curModel 是本轮实际出站的模型（6004 降级 / 本地限流降级换模型后
// 已 != 请求的原模型），requested 是解析后的原模型，incoming 是请求体原文（可能带
// cn:/global: 前缀）。上游按「账号 × 模型」发配额，所以用量/成本/缓存告警都必须跟着
// **实际出站**的模型走：
//   - 记成请求的原模型 → 原模型虚高（模型限流映射页出现 941.9M / 600M 这类超额）、
//     兜底模型虚低，推算式推算与请求判定跟着一起错；
//   - 不剥前缀 → 成本账本（pool.NoteModelCost）与倍率表（upstream.ModelRate）的键
//     都按裸名索引，带前缀查不到。
func attemptModel(cur, requested, incoming string) string {
	for _, name := range []string{cur, requested, incoming} {
		if strings.TrimSpace(name) == "" {
			continue
		}
		_, bare := upstream.SplitRealmPrefix(name)
		return bare
	}
	return ""
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
