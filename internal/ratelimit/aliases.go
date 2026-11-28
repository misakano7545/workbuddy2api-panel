package ratelimit

import "strings"

// aggregateModelIDs 客户端菜单里的「分类汇总」别名：三个速度预设、主力/默认入口，
// 以及自动模式开关本身。它们不是可调用的模型：
//
//   - deep-model / auto-chat 是 IDE/CLI 族独有 id，chat 端点直接回 11102
//     「service info not found」（internal/upstream/global_models.go 的实测记录）；
//   - fast-model / balanced-model / primary-model / default-model 是客户端把一组
//     具体模型折叠成的入口，请求路径落到的是折叠前的那个模型。
//
// 限流页把它们列出来只会占一行配额、还总是排在前面挡视线（用户 2026-11-28 反馈），
// 所以整类从页面与降级链里剔掉：既不是「黑名单」（那是运营手动拉黑、可解除），
// 也不该出现在「可用模型」里。
//
// 与 internal/upstream/clientpick.go 的 pickPresetIDs / autoModeIDs 同源；本包不
// import upstream（下游 fork 只做自包含改动），故在此另立一份判据。新增预设 id 时
// 两处都要加。
var aggregateModelIDs = map[string]bool{
	"auto":           true,
	"auto-chat":      true,
	"default-model":  true,
	"fast-model":     true,
	"balanced-model": true,
	"deep-model":     true,
	"primary-model":  true,
}

// isAggregateModel 报告 bare 名（已剥域前缀）是否是分类汇总别名。上游 id 大小写
// 不敏感，这里按小写比较。
func isAggregateModel(bare string) bool {
	return aggregateModelIDs[strings.ToLower(strings.TrimSpace(bare))]
}
