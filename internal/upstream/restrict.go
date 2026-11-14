// restrict.go 上游 11140「账号被风控限制」的识别（风控连坐保护的判据，见 server/restrict.go）。
//
// 11140 这个 code 承载了几类处置方向相反的错误，必须分开：
//   - "request illegal"（auth_forbidden，账号级授权风控）→ accountFaultMarkers；
//   - 账号风控限制文书（「账号存在异常行为，已被限制使用」/ "unusual activity"）→ 本文件。
//     这一类在上游判在设备/出口上，换账号只会连坐（workbuddy2api-hub #422 实测：一次任务
//     7 个号全封）；
//   - 模型级限流（"The model provider is rate-limiting requests."）→ softRateMarkers；
//   - 内容安全审核（"content failed safety review"）→ contentBlockedMarkers。
//
// 所以必须 code + 文案同时成立，且文案只认「账号」限制的专属措辞：只按 code 判会把限流
// 转成账号终态（禁用一个好号）；泛化措辞「已被限制」同样可能出现在模型/功能级的限制
// 文案里（例如「该模型已被限制使用」），按它判会把模型级限制误当成设备级扫荡、
// 停掉整个域的派发。
package upstream

import "strings"

// restrictionMarkers 账号风控限制文书的专属措辞（hub #422 的 403 样例）。
// 英文形态按小写比对（上游大小写不固定），中文原样比对。
var restrictionMarkers = []string{
	"账号存在异常行为",
	"unusual activity",
}

// IsRestricted 报告响应信封是否为「账号被上游风控限制」（code 11140 + 限制文书）。
func IsRestricted(body string) bool {
	if !has11140(body) {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range restrictionMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// IsAccountRestriction 报告 ErrAccountFault 的 body 是否属于 11140 账号限制的任一形态
// （"request illegal" 授权风控，或限制文书）。两者在上游都是设备/出口级判据：调用方禁用
// 该号、停止换号，并把这次限制计入连坐判定。两种形态都要求 code 11140：同一句
// "request illegal" 也出现在 11-128 渠道校验（请求级、可自愈）等别的 code 上，按文案判
// 就会让一次请求级拦截停掉轮转、还可能凑成连坐暂停整个域。
func IsAccountRestriction(body string) bool {
	if IsRestricted(body) {
		return true
	}
	return has11140(body) && strings.Contains(strings.ToLower(body), "request illegal")
}

// has11140 结构化 code 11140（含帧内 error.code 与字符串形态的 code）。
func has11140(body string) bool {
	return hasBusinessCode(body, "11140") || HasCodeMarker(body, "11140")
}
