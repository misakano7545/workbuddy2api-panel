// images.go 文生图 / 图生图：POST {chatBase}/v2/images/generations 与 /v2/images/edits。
//
// 实测（带池内真实 token）：上游只有这两条图像路由——/v1/images/generations、
// /v2/image/generations、/v2/plugin/images/generations、/v2/chat/images/generations 全
// `{"error_msg":"404 Route Not Found"}`。请求 {model,prompt,n,size}：size 生效（默认
// 1536x1536）、n 被忽略（恒 1 张）、未知键被忽略；回程信封 data 已是 OpenAI 形状
// {created,data:[{url}],usage{...,credit}}，图是签名 COS 直链（约 24h 有效）——网关只拆
// 信封，不转存字节。edits 多一个 image（data URL 数组）。
//
// 模型名按 realm 不同：cn = hunyuan-image-alpha（≈5.7 credit/张，与尺寸无关）；
// global = gpt-image-2.5-sunburst（混元在国际版无路由，回 14401 route config not found）。
package upstream

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 图像通道（chatBase 域，与 growth/report 同 base 同头）。
const (
	imagesPath = "/v2/images/generations"
	editsPath  = "/v2/images/edits"
)

// ImageGenerations 文生图，返回上游 data 对象（OpenAI 形状，可直接透出）。
// 复用 growthJSON：同 chatBase、同 BillingHeaders、同信封解码与 *Error 语义。
func (c *Client) ImageGenerations(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, imagesPath, body)
}

// ImageEdits 图生图（image 为 data URL 数组），同样返回 OpenAI 形状的 data。
func (c *Client) ImageEdits(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, editsPath, body)
}

// stashImageModels 从一次目录解析里留存图像模型（在 nonChatModel 过滤前调用，零额外请求）。
// 存 Client 上（不是包级全局）：测试各建各的 Client，互不串味。
func (c *Client) stashImageModels(realm string, entries map[string]ModelInfo) {
	var out []ModelInfo
	for _, m := range entries {
		if !imageModelTags(m.Tags) {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return
	}
	c.imageMu.Lock()
	if c.imageModels == nil {
		c.imageModels = map[string][]ModelInfo{}
	}
	c.imageModels[realm] = out
	c.imageMu.Unlock()
}

// ImageModels 最近一次目录解析留存的图像模型（空 = 尚未探测过该域）。
// 目录把这些模型从**对话**列表剔掉（客户端拿它发对话会 11102），留存只供
// /v1/models 以 image_generation=true 标记列出——客户端据此发现可用的出图模型名。
func (c *Client) ImageModels(realm string) []ModelInfo {
	c.imageMu.Lock()
	defer c.imageMu.Unlock()
	return c.imageModels[realm]
}

// imageModelTags 判定模型是否出图（上游 tag 两态：text-to-image 出图、image-to-image 改图）。
func imageModelTags(tags []string) bool {
	for _, t := range tags {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "text-to-image", "image-to-image":
			return true
		}
	}
	return false
}
