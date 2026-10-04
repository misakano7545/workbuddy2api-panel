// images.go 文生图：POST {chatBase}/v2/images/generations。
//
// 实测（带池内真实 token）：上游只有这一条图像路由——/v1/images/generations、
// /v2/image/generations、/v2/plugin/images/generations、/v2/chat/images/generations 全
// `{"error_msg":"404 Route Not Found"}`。请求 {model,prompt,n,size}：size 生效（默认
// 1536x1536）、n 被忽略（恒 1 张）、未知键被忽略；回程信封 data 已是 OpenAI 形状
// {created,data:[{url}],usage{...,credit}}，图是签名 COS 直链（约 24h 有效）——网关只拆
// 信封，不转存字节。
//
// 模型名按 realm 不同：cn = hunyuan-image-alpha（≈5.7 credit/张，与尺寸无关）；
// global = gpt-image-2.5-sunburst（混元在国际版无路由，回 14401 route config not found）。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// imagesPath 文生图通道（chatBase 域，与 growth/report 同 base 同头）。
const imagesPath = "/v2/images/generations"

// ImageGenerations 文生图，返回上游 data 对象（OpenAI 形状，可直接透出）。
// 复用 growthJSON：同 chatBase、同 BillingHeaders、同信封解码与 *Error 语义。
func (c *Client) ImageGenerations(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, imagesPath, body)
}
