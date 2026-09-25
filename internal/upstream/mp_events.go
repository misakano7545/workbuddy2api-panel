// mp_events.go 小程序埋点指纹。Sequential_* 成长任务仍走这套上报。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const mpReportPath = "/v2/report"

// mpEventBase 小程序埋点公共指纹（appservice wQ()+Ao() 对齐）。
func mpEventBase(a *auth.Auth) map[string]any {
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    "0655736a-607f-4d9d-b430-58176ee9a090",
		"timezone":     "Asia/Shanghai",
		"userId":       a.UID,
		"userNickname": a.Nickname,
	}
}

// ReportMPEvent 以小程序指纹向 www.codebuddy.cn/v2/report 批量上报事件。
func (c *Client) ReportMPEvent(a *auth.Auth, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("mp report: no events")
	}
	base := mpEventBase(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.BillingBaseCN+mpReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	_, err = c.doJSON(req)
	return err
}

// MiniChatSendEvent 构造一条小程序 chat_request_send（Sequential 对话任务计数），
// 不带 activityId。
func MiniChatSendEvent(conversationID string) map[string]any {
	rid := "wb2api-" + clientToken()
	return map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + rid[len(rid)-8:],
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
}

// MiniExpertUseEvent 构造 Sequential_Tasks_2 的判据事件：mp 指纹 expert_actual_use。
// 形状对齐小程序源码真实发射点（上报即 completed，claim +200c+5e）。
// 不带 conversationId/activityId；extVersion 用小程序自身版本 2.2.8；
// source=mini_program + type 固定 "send_message"。
// expertID 必须是专家市场真实 ex_ id，空 id 服务端不入账。
func MiniExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}
	if expertName == "" {
		expertName = expertID
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8", "source": "mini_program",
		"id": expertID, "name": expertID,
		"expertTitle": expertName, "type": "send_message",
		"characterCount": 12, "expertType": expertType,
	}
}

// MiniChatModelEvent mp 对话事件 + 模型字段（Sequential_Tasks_5 判据载体）。
func MiniChatModelEvent(conversationID, modelID, modelName string) map[string]any {
	ev := MiniChatSendEvent(conversationID)
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// MiniPlaybookEvents mp 指纹灵感事件组（Sequential_Tasks_7 判据载体）。
func MiniPlaybookEvents(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
		"skills": "", "skillNames": "",
	}
	cta := map[string]any{
		"eventCode": "playbook_cta_click", "source": "discover", "position": 1,
		"extVersion": "2.2.8",
	}
	for k, v := range base {
		cta[k] = v
	}
	send := map[string]any{
		"eventCode": "playbook_prompt_send", "source": "discover",
		"promptLength": 96, "isOfficial": 1,
		"conversationId": "wb2api-mp-pb-" + clientToken(),
		"extVersion":     "2.2.8",
	}
	for k, v := range base {
		send[k] = v
	}
	return []map[string]any{cta, send}
}
