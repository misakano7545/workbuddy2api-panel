// report.go growth 域「对话活跃上报」接口：POST {chatBase}/v2/report。
//
// 通道与形状以 **WorkBuddy 5.9.3 真机**为准（2026-10-28 抓包）：客户端把 /v2/report
// 发到 chat 域 www.workbuddy.cn（不是 billing 域 codebuddy.cn），UA 是 WorkBuddy
// 三段式（不是 billing 的 axios/1.16.1），事件里带全套设备/包指纹。
// 照抄客户端 chat_request_send 事件形状（勿用最小 3 字段，防上游后续加严）。
// 事件必须带 userId（=账号 uid），缺失则服务端 200 但静默丢弃（实测见
// REPORT-active-map.md §2）。
//
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务（领养前置）。
// 风控口径：每号每天 1 次即可（activity_hours 单时点），不做多时点高频上报。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
)

// reportPath 活跃上报通道（实测）。
const reportPath = "/v2/report"

// billingJSONRaw 同 billingJSON，另外把信封与**原始响应体**一并交回。
// 为什么需要：daily-checkin 的成功回执是**平铺**的（没有 {code,msg,data} 信封），
// 只交 env.Data 会拿到 null，积分数就丢了（见 checkin_receipt.go 头注释）。
func (c *Client) billingJSONRaw(a *auth.Auth, method, path string, body any) (apiEnvelope, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return apiEnvelope{}, nil, err
		}
		rdr = bytes.NewReader(raw)
	} else if method == http.MethodPost {
		// 无 body 的 POST 发 `{}`：5.9.3 真机 billing 族 POST（checkin-activity-status
		// 等）Content-Length 恒为 2、body 是 `{}`，不是空 body。
		rdr = strings.NewReader("{}")
	}
	req, err := http.NewRequest(method, c.billingBase(a)+path, rdr)
	if err != nil {
		return apiEnvelope{}, nil, err
	}
	c.BillingHeaders(req, a)
	return c.doJSONRaw(req, a)
}

// billingJSON 发 billing 域（billingBase，codebuddy.cn）请求并解信封；body 为 nil 时不带请求体。
// 与 travel.go 的 growthJSON 对称（growth 域走 chatBase + BillingHeaders；billing 域走 billingBase）。
// report/checkin 等 billing 端点共用：请求头统一 BillingHeaders，信封与错误语义同 doJSON。
func (c *Client) billingJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	env, _, err := c.billingJSONRaw(a, method, path, body)
	return env.Data, err
}

// billingMeterJSON 仅对 /billing/meter 族端点（get-user-resource / daily-checkin）
// 按 realm 走双路径 fallback：global 先无 /v2 前缀，ErrNotFound 时二次换有 /v2 前缀
// （上游新旧路径分叉）；cn 单路径（有 /v2）现状不变。仅 global realm 才有多路径。
func (c *Client) billingMeterJSON(a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	env, _, err := c.billingMeterRaw(a, paths, method, body)
	return env.Data, err
}

// billingMeterRaw 同 billingMeterJSON，另外把信封与原始 body 一并交回（签到回执要用
// 平铺 body 里的 credit，见 checkin_receipt.go）。
func (c *Client) billingMeterRaw(a *auth.Auth, paths []string, method string, body any) (apiEnvelope, []byte, error) {
	var (
		lastErr error
		lastRaw []byte
	)
	for i, path := range paths {
		env, raw, err := c.billingJSONRaw(a, method, path, body)
		if err == nil {
			return env, raw, nil
		}
		lastErr, lastRaw = err, raw
		// 仅 404 换路径（路径不存在才值得 fallback）；其他错误直接返回。
		var ue *Error
		if !errors.As(err, &ue) || ue.Kind != ErrNotFound || i == len(paths)-1 {
			return env, raw, err
		}
	}
	return apiEnvelope{}, lastRaw, lastErr
}

// billingRetryDelay 签到/余额等维护类计费调用瞬时错误重试的间隔基数。
// 独立变量供测试缩短（生产固定 2s：第 1 次重试等 2s、第 2 次等 4s）。
var billingRetryDelay = 2 * time.Second

// billingProcessingRetryDelays 429「请求处理中」的退避阶梯：这是上游**临时状态**
// （签到/余额查询偶发），比 5xx 需要更久沉降才能结算完；与真·限流的退避分开，
// 免得把限流也拖成三次长重试。口径移植自 cli2api 的 WorkBuddy 修复（519a529）。
var billingProcessingRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

// billingProcessingMarkers 上游 429 里表示「请求处理中」的文案（中英双形态）。
var billingProcessingMarkers = []string{"请求处理中", "request is being processed", "request processing"}

// isBillingProcessingErr 报告 err 是否为「429 + 请求处理中」形态：临时状态，值得等它沉降。
// 只认带分类的 *Error 且 Status==429；普通 429（真·限流）不匹配，仍不重试。
func isBillingProcessingErr(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(ue.Msg)
	for _, m := range billingProcessingMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// IsTransientBilling 导出「这次计费类失败值不值得过一会儿再试」的判据，供调度器
// 决定要不要把账号放进延后补签队列（签到本身已有 2s/4s 两次内联重试，覆盖不了
// 上游「请稍后重试」这种需要分钟级沉降的形态）。
func IsTransientBilling(err error) bool { return isTransientBillingErr(err) }

// isTransientBillingErr 报告 err 是否值得对计费维护类调用做有界重试：
// 上游 5xx（ErrServer，实测偶发 "code 10000 / API request failed with status
// code: 500"）、网络层错误（非 *Error 的传输失败），以及 429「请求处理中」
// （临时状态）。业务错误（code!=0 的已签到/参数错、其余 4xx、真·限流）不重试
// ——重试只会原样再失败一次。
func isTransientBillingErr(err error) bool {
	if err == nil {
		return false
	}
	if isBillingProcessingErr(err) {
		return true
	}
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Kind == ErrServer
	}
	return true
}

// retryBillingTransient 对签到/余额这类低频维护调用做瞬时错误有界重试。两类瞬时
// 错误、两套退避：429「请求处理中」按 2s/5s/10s 最多补打 3 次（上游结算需要更久）；
// 其余瞬时错误沿用 2s/4s 最多补打 2 次。首次成功或非瞬时错误立即返回；类别中途
// 变化（处理中 → 5xx）按新类别继续。chat 热路径不用本策略——它有自己的换号轮转
// 语义，重试会放大在途请求。
func (c *Client) retryBillingTransient(fn func() error) error {
	err := fn()
	for used := 0; err != nil && isTransientBillingErr(err); used++ {
		processing := isBillingProcessingErr(err)
		max := 2
		if processing {
			max = len(billingProcessingRetryDelays)
		}
		if used >= max {
			break
		}
		if processing {
			time.Sleep(billingProcessingRetryDelays[used])
		} else {
			time.Sleep(time.Duration(used+1) * billingRetryDelay)
		}
		err = fn()
	}
	return err
}

// chatRequestEvent 客户端 chat_request_send 事件（5.9.3 真机字段集，逐字对齐）。
//
// 2026-10-28 国内版 5.9.3 真机抓取 /v2/report 事件（body 里 4 类事件：chat_request_send /
// chat_message_response / chat_message_status / chat_request_response）。本结构只承担
// chat_request_send 的**chat 专属**字段；设备/包指纹由 desktopFingerprint(a) 合并进来
// （ideName / ideType / ideVersion / commit / releaseDate / machineId / sessionId /
// extName / extVersion / os / arch / osVersion / cpuModel / cpuCores / memorySize /
// qimei36 / userId / username / userNickname / product / timezone）。
//
// 与旧实现的差异（每一处都是「多发或漏发」造成的形态偏差）：
//   - 删掉 maxToken / maxSteps / temperature / maxRetries / skillCount / totalCount /
//     fileUri / expertId —— 5.9.3 的 chat_request_send 里**没有这些键**（旧结构是从
//     更早的客户端抄的，多出来的空值键反而显眼）。
//   - 补 isMaxMode / source / inputAttemptId / vcsType / vcsRepo / vcsBranchName /
//     vcsRevId —— 5.9.3 必带。
//   - agentName / agentType 由 "default" / "conversation" 改为 **"cli" / "main"**
//     （真机桌面端走内置 CLI，两个字段都是 cli/main）。
//   - traceId 不再留空：真机有值（32 hex，与当轮 X-Trace-ID 同源）。
type chatRequestEvent struct {
	EventCode             string   `json:"eventCode"`
	Mode                  string   `json:"mode"`
	ConversationID        string   `json:"conversationId"`
	RequestID             string   `json:"requestId"`
	InputLength           int      `json:"inputLength"`
	RequestModelID        string   `json:"requestModelId"`
	RequestModelName      string   `json:"requestModelName"`
	IsPlan                bool     `json:"isPlan"`
	IsAutoExecuteTerminal bool     `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool     `json:"isAutoModify"`
	CodebaseEnable        bool     `json:"codebaseEnable"`
	IsMaxMode             bool     `json:"isMaxMode"`
	Source                string   `json:"source"`
	MentionContexts       []any    `json:"mentionContexts"`
	KnowledgeID           []any    `json:"knowledgeId"`
	KnowledgeName         []any    `json:"knowledgeName"`
	CodebaseID            string   `json:"codebaseId"`
	MentionContextCount   int      `json:"mentionContextCount"`
	Command               string   `json:"command"`
	InputAttemptID        string   `json:"inputAttemptId"`
	RecommendID           string   `json:"recommendId"`
	SkillID               string   `json:"skillId"`
	TraceID               string   `json:"traceId"`
	RootRequestID         string   `json:"rootRequestId"`
	ParentConversationID  string   `json:"parentConversationId"`
	AgentName             string   `json:"agentName"`
	AgentType             string   `json:"agentType"`
}

// reportChatEventMap 把 chat 专属字段与设备/包指纹拼成一条完整事件（真机键集）。
func reportChatEventMap(a *auth.Auth, ev chatRequestEvent) map[string]any {
	m := desktopFingerprint(a) // 设备 + 包指纹（含 qimei36 / commit / releaseDate / cpu 等）
	m["eventCode"] = ev.EventCode
	m["mode"] = ev.Mode
	m["conversationId"] = ev.ConversationID
	m["requestId"] = ev.RequestID
	m["inputLength"] = ev.InputLength
	m["requestModelId"] = ev.RequestModelID
	m["requestModelName"] = ev.RequestModelName
	m["isPlan"] = ev.IsPlan
	m["isAutoExecuteTerminal"] = ev.IsAutoExecuteTerminal
	m["isAutoModify"] = ev.IsAutoModify
	m["codebaseEnable"] = ev.CodebaseEnable
	m["isMaxMode"] = ev.IsMaxMode
	m["source"] = ev.Source
	m["mentionContexts"] = ev.MentionContexts
	m["knowledgeId"] = ev.KnowledgeID
	m["knowledgeName"] = ev.KnowledgeName
	m["codebaseId"] = ev.CodebaseID
	m["mentionContextCount"] = ev.MentionContextCount
	m["command"] = ev.Command
	m["inputAttemptId"] = ev.InputAttemptID
	m["recommendId"] = ev.RecommendID
	m["skillId"] = ev.SkillID
	m["traceId"] = ev.TraceID
	m["rootRequestId"] = ev.RootRequestID
	m["parentConversationId"] = ev.ParentConversationID
	m["agentName"] = ev.AgentName
	m["agentType"] = ev.AgentType
	// VCS 段：真机恒带四个键（未知仓库时为 unknown + 三个空串），缺了是可判别形态。
	m["vcsType"] = "unknown"
	m["vcsRepo"] = ""
	m["vcsBranchName"] = ""
	m["vcsRevId"] = ""
	// buddy 段 + OTel 语义键：真机每个事件都带，buddyId/Name 为空串。
	m["buddyId"] = ""
	m["buddyName"] = ""
	m["codebuddy.session_id"] = ev.ConversationID
	m["codebuddy.conversation_request_id"] = ev.RequestID
	return m
}

// ReportChatActivity 向上游发送一条对话活跃上报（chat_request_send）。
// conversationID 由调用方生成（如 wb2api-<ms>），无需真实会话——服务端不校验一致性。
// requestID 为本轮请求独立标识（多轮同会话上报时各条不同）；空时回落 conversationID。
// 错误语义与 doJSON 一致：HTTP 非 2xx / 业务 code != 0 → *Error。
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID, requestID string) error {
	return c.ReportChatActivityModel(a, conversationID, requestID, "deepseek-v4.1-flash", "DeepSeek-V4.1-Flash")
}

// ReportChatActivityModel 同上，但可指定上报携带的模型：供「体验某模型」类任务
// 对齐实际模型（如 Model_chat_GLM5.2 需 requestModelId=glm-5.2 与独立 requestID）。
//
// 通道：**chatBase**（www.workbuddy.cn）+ 客户端 report 头族。
// 旧实现把这条上报发到 billingBase（codebuddy.cn）并用 billing 的 axios UA——5.9.3
// 真机 /v2/report 走的是 chat 域、UA 是 WorkBuddy 三段式，两处都按客户端改。
func (c *Client) ReportChatActivityModel(a *auth.Auth, conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4.1-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	traceID := session.NewTraceID()
	ev := chatRequestEvent{
		EventCode:             "chat_request_send",
		Mode:                  "craft",
		ConversationID:        conversationID,
		RequestID:             requestID,
		InputLength:           12,
		RequestModelID:        modelID,
		RequestModelName:      modelName,
		IsPlan:                false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:          false,
		CodebaseEnable:        false,
		IsMaxMode:             true,
		Source:                "work",
		MentionContexts:       []any{},
		KnowledgeID:           []any{},
		KnowledgeName:         []any{},
		CodebaseID:            "",
		MentionContextCount:   0,
		Command:               "",
		InputAttemptID:        session.NewConversationID(),
		RecommendID:           "",
		SkillID:               "",
		TraceID:               traceID,
		RootRequestID:         requestID,
		ParentConversationID:  conversationID,
		AgentName:             "cli",
		AgentType:             "main",
	}
	raw, err := json.Marshal([]map[string]any{reportChatEventMap(a, ev)})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+reportPath, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	// 头族逐字对齐真机 /v2/report（2026-10-30 双版本，国际 37 条 + 国内多条）：
	//   Accept: application/json, text/plain, */*
	//   Content-Type: application/json;charset=UTF-8
	//   X-Request-ID / X-Product: SaaS / User-Agent 三段式 / Authorization / X-User-Id / X-Domain
	//
	// **不发 X-Requested-With**：真机 /v2/report 按 UA 分两族，只有渲染器侧那族
	// （UA 首段驼峰 `WorkBuddy/<ver> <brand>/<ver> …`）带它；本函数发的三段式
	// （`<brand>/<ver> <brand>/<ver> …`）一族**没有**。两族头各取一半就是第三族。
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("X-Request-ID", session.NewMessageID())
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", desktopUAFor(a))
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Domain", c.chatHost(a))
	// 设备风控头：真机 /v2/report 不带 X-Device-Token（只有 billing 族带），故不注入。
	_, err = c.doJSON(req, a)
	return err
}
