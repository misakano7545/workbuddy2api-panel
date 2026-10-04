// images.go 文生图端点 POST /v1/images/generations（OpenAI 形状）。
//
// 上游图像模型不进 /v1/models（目录按 tag text-to-image 剔除，见 nonChatModel），所以
// 模型名不走目录校验，直接按 realm 前缀路由到对应账号池：
//
//	cn:hunyuan-image-alpha         国内版（实测 200 + 签名 COS URL）
//	global:gpt-image-2.5-sunburst  国际版（混元在国际版无路由，回 14401）
//
// 请求体只把 model 换成裸名（前缀是网关路由协议，上游只认裸名），其余原样透传；回程把
// 上游 data 原样写出——它已经是 {created,data:[{url}],usage}，不转存、不改写。
// ponytail: 单次尝试、不换号不冷却（图像按次人工触发、量小）；要抗故障再接 chat 路径的
// 轮转与 applyErrorPolicy。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// defaultImageModel 请求未带 model 时的缺省图像模型（国内版；realm 由前缀决定）。
const defaultImageModel = "cn:hunyuan-image-alpha"

func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"prompt is required（例：{\"model\":\""+defaultImageModel+"\",\"prompt\":\"一只猫\"}）")
		return
	}
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = defaultImageModel
	}
	realm, bare := resolveModel(model)
	req["model"] = bare
	out, err := json.Marshal(req)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	a := h.cfg.Pool.PickExcludingForRealm(map[string]bool{}, bare, realm)
	if a == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_account",
			"没有可用的 "+realm+" 账号（图像只在 "+realm+" 域出图）")
		return
	}
	data, err := h.cfg.Upstream.ImageGenerations(a, json.RawMessage(out))
	if err != nil {
		status := http.StatusBadGateway
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Status >= 400 {
			status = ue.Status // 上游状态码（如 14401 的 400）比一律 502 更能说明问题
		}
		log.Printf("images %s model=%s: %v", logfmt.Label(a.UID, a.Nickname), bare, err)
		writeOpenAIError(w, status, "upstream_error", err.Error())
		return
	}
	log.Printf("images %s model=%s: 出图 ok（%dB）", logfmt.Label(a.UID, a.Nickname), bare, len(data))
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
