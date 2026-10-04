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
	"sort"

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

// stashGenerationModels 从一次目录解析里留存媒体生成模型（在 nonChatModel 过滤**前**
// 调用，零额外请求），按 GenerationKind 标 image/video。存 Client 上（不是包级全局）：
// 测试各建各的 Client，互不串味。
//
// **按 ID 合并而非覆盖**：global 目录是多 UA 并发探测的，宽目录（WorkBuddy 产品身份，
// 29 个模型）与窄目录（13 个）都会来这里落一笔——覆盖式留存会让结果取决于哪个探测先
// 返回，表现为「这次面板有 gpt-image-2.5-sunburst、下次变成 hunyuan-image-alpha」。
// ponytail: 只增不减，上游下架的模型要等重启才消失；出现真实误报再加版本比对。
func (c *Client) stashGenerationModels(realm string, entries []ModelInfo) {
	media := make([]ModelInfo, 0, len(entries))
	for _, m := range entries {
		if kind := generationKind(m.Tags); kind != "" {
			m.GenerationKind = kind
			media = append(media, m)
		}
	}
	if len(media) == 0 {
		return
	}
	c.imageMu.Lock()
	defer c.imageMu.Unlock()
	if c.imageModels == nil {
		c.imageModels = map[string][]ModelInfo{}
	}
	merged := map[string]ModelInfo{}
	for _, m := range c.imageModels[realm] {
		merged[m.ID] = m
	}
	for _, m := range media {
		merged[m.ID] = m
	}
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, merged[id])
	}
	c.imageModels[realm] = out
}

// sortedInfos 把模型 map 摊成稳定顺序的切片（map 迭代序随机，留存顺序不该抖动）。
func sortedInfos(byID map[string]ModelInfo) []ModelInfo {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

// GenerationModels 最近一次目录解析留存的出图/出视频模型（空 = 尚未探测过该域）。
// 目录把这些模型从**对话**列表剔掉（客户端拿它发对话会 11102），留存只供 /v1/models
// 与面板以 image_generation / video_generation 标记列出——客户端据此发现可用的模型名。
func (c *Client) GenerationModels(realm string) []ModelInfo {
	c.imageMu.Lock()
	defer c.imageMu.Unlock()
	return c.imageModels[realm]
}
