// restrict.go 上游 11140「账号存在异常行为」的识别（风控连坐保护的判据）。
//
// 背景：11140 有两种形态，处置方向完全相反，必须分开：
//   - 英文 "request illegal"（auth_forbidden，渠道校验/授权）→ accountFaultMarkers；
//   - 账号风控限制文书（中文「账号存在异常行为，已被限制使用」/ 英文 "unusual activity"）
//     → 本文件。这一类判据在上游是**设备/出口级**，换账号只会连坐（hub #422 实测：
//     一次任务 7 个号全封）。
//
// 为什么必须 code + 文案同时要求：11140 也承载模型级限流（"The model provider is
// rate-limiting requests."），只按 code 判会把限流转成账号终态（少服务一天）；只按
// 文案判会误伤正文里恰好讨论这些词的数据。
package upstream

import "strings"

// restrictionMarkers 风控限制文书的实测措辞（社区封禁复盘 + hub #422 的 403 样例）。
var restrictionMarkers = []string{
	"账号存在异常行为",
	"已被限制",
	"unusual activity",
}

// IsRestricted 报告响应信封是否为「账号被上游风控限制」（结构化 code 11140 + 限制文案）。
func IsRestricted(body string) bool {
	if !hasBusinessCode(body, "11140") && !HasCodeMarker(body, "11140") {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range restrictionMarkers {
		if strings.Contains(body, m) || strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
