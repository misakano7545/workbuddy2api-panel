// desktop.go 桌面客户端（WorkBuddy Desktop 5.5.6）行为指纹上报。
//
// 来源：2026-09-12 Sunny 抓包实测（data/desktop-task-protocol.md）。桌面端点亮
// 「需电脑端」类任务的关键不是独立端点，而是同一 POST /v2/report 通道上
// **不同的客户端指纹**：
//
//	POST https://copilot.tencent.com/v2/report        ← chatBase（CLI 上报走 billingBase）
//	User-Agent: WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1
//	X-Domain: copilot.tencent.com, X-Product: SaaS, X-User-Id: <uid>
//	Body: [ {...event...} ]                           ← 数组
//
// 每个事件除业务字段外必带桌面指纹（ideName/ideType=WorkBuddy、
// extName=workbuddy-desktop 等）。实测点亮记录：
//   - RichMeow_Chat（桌面端对话1次）：agent_task_created + 成功的
//     chat_message_response(isSuccessful=true) → 1/1 + UR Buddy。
//   - Hp_Appearance（主题任务）：独立 API
//     POST /v2/user-asset/appearance/set {kind:"theme",resource_key} → SetAppearanceTheme。
//
// 注意：service 端对事件链有一定真实性校验倾向（RichMeow 需要消息成功回执），
// 本模块按实测事件形状发送，不保证所有任务都能 API 侧点亮——autotask 侧
// 仍按 attempt 语义处理结果。
package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
)

const (
	desktopReportPath    = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"
	// desktopVersion / desktopCommit / desktopReleaseDate 官方分发包实测值
	// （install-manifest.json 的 appVersion + /v2/report body 的 commit/releaseDate）。
	// 与面板自身 client_version 分开：/v2/report 是「桌面端行为」指纹，应恒为桌面端版本。
	//
	// **按 realm 分开**（2026-10-30 双版本抓包实测）：两版上报的版本三元组不同，
	// 且 commit/releaseDate 是打包期常量、无法从版本号推导：
	//
	//	CN 5.9.3     ideVersion 5.9.3 commit d4692421… releaseDate 1793093056315
	//	global 5.9.4 ideVersion 5.9.4 commit fa0a0901… releaseDate 1793288804029
	//
	// CN 三元组在 2026-10-28 按 5.9.3 真机 /v2/report 重新采集（commit / releaseDate /
	// ideVersion / extVersion 逐字取自 7 条上报事件，与分发包 cli/product.json 的
	// commit + date 交叉一致）：
	//   commit      = d46924219e79332f49c37ccebf27292f87001b19
	//   releaseDate = 1793093056315
	//
	// 此前不分档、恒用 global 的三元组，等于给每个国内号都盖了国际版的包指纹
	// （版本号与安装清单自相矛盾），是最容易被设备侧写识破的一处。
	desktopVersionCN         = "5.9.3"
	desktopCommitCN          = "d46924219e79332f49c37ccebf27292f87001b19"
	desktopReleaseDateCN     = int64(1793093056315)
	// global 三元组按 5.9.4 国际分发包重采（2026-10-30 拆包）：
	//   cli/product.json commit = fa0a0901906addaa1a4259f6df347388a5563ded
	//   cli/product.json date   = 2026-10-29T15:46:44.029Z → 1793288804029
	//   真机 /v2/update?platform=workbuddy-win32-x64-user&version=5.9.4 与 UA 段一致。
	desktopVersionGlobal     = "5.9.4"
	desktopCommitGlobal      = "fa0a0901906addaa1a4259f6df347388a5563ded"
	desktopReleaseDateGlobal = int64(1793288804029)
)

// desktopBase 桌面端 /v2/report 与 user-asset 走 chatBase（copilot.tencent.com）。
func (c *Client) desktopBase(a *auth.Auth) string { return c.chatBase(a) }

// desktopUAFor 桌面端 UA：两段品牌一致（与业务链路同源，都是 SDK 的 buildUserAgent）。
// CN 5.9.3 → `WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0`（真机实测）；
// global 5.9.4 → `workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0`（真机实测）。
func desktopUAFor(a *auth.Auth) string {
	ver, _, _ := desktopTriple(a)
	brand := uaBrandFor(a)
	cli := defaultCliVersion
	return brand + "/" + ver + " " + brand + "/" + ver + " CLI/" + cli
}

// desktopTriple 返回该账号域的桌面端 (ideVersion, commit, releaseDate) 三元组。
// 两版分发包不同 → 三元组不同；nil（默认 CN 形态）走 CN 值。
func desktopTriple(a *auth.Auth) (ver, commit string, release int64) {
	if a != nil && a.IsGlobal() {
		return desktopVersionGlobal, desktopCommitGlobal, desktopReleaseDateGlobal
	}
	return desktopVersionCN, desktopCommitCN, desktopReleaseDateCN
}

// desktopHost 返回 X-Domain 用的裸主机名（与真机一致：www.workbuddy.ai /
// copilot.tencent.com）。从 base 里剥 scheme 与路径；解析失败时退回 base 原串，
// 保证不会因为 URL 畸形而把该头发成空串（头缺失同样会触发渠道校验）。
func (c *Client) desktopHost(a *auth.Auth) string {
	base := c.desktopBase(a)
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"), "/")
}

// deriveID 由 uid 稳定派生一个 36 位 hex 设备标识（machineId/qimei36 复用），
// 幂等：同一账号每次生成相同值，模拟固定设备。
func deriveID(a *auth.Auth, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + a.UID))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// DesktopEvent 桌面端事件：业务字段任意（map），公共指纹由 ReportDesktopEvent 注入。
type DesktopEvent map[string]any

// desktopFingerprint 公共桌面指纹字段（注入每个事件，覆盖同名业务键）。
// 版本三元组按 realm 取（见 desktopTriple）；qimei36 仅国内版有（见下）。
func desktopFingerprint(a *auth.Auth) map[string]any {
	now := time.Now().UnixMilli()
	ver, commit, release := desktopTriple(a)
	fp := map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       a.UID,
		"username":     a.Nickname,
		"userNickname": a.Nickname,
		"product":      "SaaS",
		"releaseDate":  release,
		"commit":       commit,
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   ver,
		"machineId":    deriveID(a, "machine"),
		"sessionId":    deriveID(a, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   ver,
		"os":           "win32",
		"arch":         "x64",
		// osVersion / cpuModel / cpuCores / memorySize 是真机上报名（5.8.4 实测）：
		// 这些字段上游侧用于设备侧写，缺 cpuModel 是最明显的「非桌面端」特征。
		"osVersion":  "10.0.28000",
		"cpuModel":   "11th Gen Intel(R) Core(TM) i5-11320H @ 3.20GHz",
		"cpuCores":   8,
		"memorySize": 16,
		"timestamp":  now,
		"presentAt":  now,
	}
	// qimei36：腾讯设备标识（36 位小写十六进制）。2026-10-20 双版本抓包实测——
	// 国内版每个 report 事件都带，国际版**一条都不带**（CN 28 键 vs GL 27 键，
	// 差集恰好只有 qimei36）。故按 realm 条件注入：给国际版补这个键反而是伪造。
	// 取值按 uid 稳定派生（同 deriveID 口径），保证同号跨重启恒同值。
	if a != nil && !a.IsGlobal() {
		fp["qimei36"] = deriveID(a, "qimei")
	}
	return fp
}

// ReportDesktopEvent 以桌面客户端指纹向 copilot.tencent.com/v2/report 批量上报事件。
// events 为业务载荷（eventCode 等字段由调用方给出）；公共指纹自动注入，
// 业务字段优先（可用于覆盖 qimei36/machineId 等设备标识做真实设备对齐）。
func (c *Client) ReportDesktopEvent(a *auth.Auth, events ...DesktopEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("desktop report: no events")
	}
	fp := desktopFingerprint(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range fp {
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
	req, err := http.NewRequest(http.MethodPost, c.desktopBase(a)+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	// X-Requested-With：**不发**。
	//
	// 2026-10-30 双版本真机复核（国际 5.9.4 37 条 + 国内 5.9.3 多条 /v2/report，
	// 按 UA 分成两族）：
	//
	//	workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/…   ← 本函数发的就是这一族，**无** X-Requested-With
	//	WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/…      ← 渲染器侧另一条上报，带 X-Requested-With
	//
	// 旧实现在 UA 取「RPC 族」的同时又补了 X-Requested-With，等于把两族的头各取一半
	// 拼成第三族——两族都不像，比少一个头更显眼。
	req.Header.Set("User-Agent", desktopUAFor(a))
	// Accept-Encoding：真机 /v2/report 与 chat / billing 同族，四段全声明
	// （夹具 official_report_cn_head.txt，cn 抓包 report 族 1/1 命中该值）。
	// 显式声明会关闭 net/http 透明解压，响应由 doJSON→decodeResponseBody 解。
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	// X-Domain 是**主机名**不是 URL：真机实测 `X-Domain: www.workbuddy.ai`，
	// 而 desktopBase 返回的是带 scheme 的 base，直接塞进去会发成
	// `https://www.workbuddy.ai`——与本仓其它头（chat/billing 用 DomainValue 的裸域）
	// 也不自洽，是最容易被上游按「非客户端形态」识别出来的一处。
	req.Header.Set("X-Domain", c.desktopHost(a))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", session.NewMessageID())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req, a)
	return err
}

// DesktopChatSequence 构造一次「桌面端成功对话」的完整事件链
// （agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful) → chat_message_status → chat_request_response）。
// 实测该链点亮 RichMeow_Chat。conversationID/requestID/messageID 由调用方生成。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	uuid := func() string { return requestID }
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": time.Now().UnixMilli(), "traceId": uuid(),
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// SetAppearanceTheme 应用外观主题（实测：POST copilot.tencent.com/v2/user-asset/appearance/set，
// 和品主题 resource_key 为 "theme-tkmw7j"，浅色 "light"、深色 "dark"）。纯 API set 不计
// Hp_Appearance 分（需客户端切主题后真实活跃），保留供调色/还原与后续验证用。
func (c *Client) SetAppearanceTheme(a *auth.Auth, resourceKey string) error {
	body := map[string]string{"kind": "theme", "resource_key": resourceKey}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.desktopBase(a)+desktopAppearanceSet, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUAFor(a))
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req, a)
	return err
}

// DesktopBuddyAppSequence 构造「进入 Buddy 应用」五连事件（实测两账号纯 API 点亮
// Buddy_App 与 Buddy_App_QQ）：discover → show → enter_click → auth_confirm →
// bindaccount_skip。buddyID 固定用企鹅教师助手 cb_y5Dy46tPQGGWtueMxXbe
// （Buddy_App_QQ 的判据应用），同时满足 Buddy_App「进入任一应用」。
func DesktopBuddyAppSequence(buddyID, buddyName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopAutomationCreateEvent 构造「定时任务创建成功」事件（实测两账号纯 API
// 点亮 automation_1）。name 为任务名，可与真实创建语义对齐。
func DesktopAutomationCreateEvent(name string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// ReportWebEvent 以 **官方桌面端等价形态** 向 /v2/report 上报单事件。
//
// 2026-11-01 真机抓包修正（夹具 testdata/official_report_cn_head.txt）：
// 官方 /v2/report 在 CN 侧 **只有一种形态**——Node/SDK 发的，11 个头：
//
//	Accept: application/json, text/plain, */*
//	Content-Type: application/json;charset=UTF-8
//	X-Request-ID / X-Product: SaaS
//	User-Agent: WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
//	Authorization / X-User-Id / X-Domain: www.workbuddy.cn
//
// **没有** sec-ch-ua / Sec-Fetch-* / Origin / Referer / x-client-platform。
// 旧实现造的是一条「浏览器 UA + Origin + Referer」的第三族请求，官方客户端
// 从不发这种包——而且 UA 里的 `Chrome/152.0.0.0` 是不可能的值（Electron 37
// 内置 Chromium 138，真机实测 `WorkBuddy/5.9.3 Chrome/138.0.7204.251
// Electron/37.10.3`）。头与体各自自洽、彼此矛盾，是最容易被单条规则命中的形态。
//
// body 仍保留 web 事件形状（os/machineId/userAgent 等）：那条路径的业务载荷
// 没有真机样本可比对，改头不动体是本轮能拿证据支撑的最小改动。
func (c *Client) ReportWebEvent(a *auth.Auth, eventCode, pageURL, elementID, elementName string) error {
	ua := c.userAgent(a)
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": ua,
		"machineId": deriveID(a, "webmachine"), "userId": a.UID,
		"userNickname": a.Nickname, "enterpriseId": a.EnterpriseID,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	base := c.webBase(a)
	req, err := http.NewRequest(http.MethodPost, base+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	// 以下逐项照抄真机夹具，顺序与取值都不自己发挥。
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", ua)
	// X-Request-ID 用**纯随机 32 hex**（NewTraceID），不是 UUIDv7：
	// 真机 report 样本 `4891f0b179cc4959be94b2c867c16e3c` 解不出时间位，
	// 而同一客户端的 chat 请求才带毫秒前缀。两条链的 ID 规则不同。
	req.Header.Set("X-Request-ID", session.NewTraceID())
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	// Accept-Encoding：同 /v2/report 真机族（四段全声明）。
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	// X-Domain 是**裸主机名**（真机 `X-Domain: www.workbuddy.cn`），不是 URL。
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"), "/")
	if u, perr := url.Parse(base); perr == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	req.Header.Set("X-Domain", host)
	_, err = c.doJSON(req, a)
	return err
}

// ---------------------------------------------------------------------------
// 2026-09-12 第五轮：客户端 asar 逆向 + Sunny 真实样本驱动的新判据（三账号实测点亮）。
// 来源：WorkBuddy.exe 5.5.6 app.asar 渲染层事件枚举/载荷 + 抓包真实专家召唤样本
// （data/desktop-task-protocol.md §7.3）。
// ---------------------------------------------------------------------------

// DesktopTemplateUseSequence 构造「使用模板创建任务」事件组（实测 template_5 计数）：
// agent_task_created_with_template {mode,isCustomModel,id,name,requestId} +
// template_used {template_id,task_mode}，JOIN 一条完整 chat 链。
// 三账号实测：5 组（不同模板）一次上报 → 5/5 点亮。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	events = append(events,
		DesktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		DesktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
	return events
}

// DesktopPlaybookPromptSequence 构造「灵感案例做同款」事件组（实测 playbook_prompt 计数）：
// web_element_click(playbook_ctaClick) + playbook_cta_click + playbook_prompt_send
// （Dialog 发送 Prompt，带 conversationId/requestId JOIN chat 链）。三账号实测 1/1 点亮。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	events = append(events,
		DesktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}
			for k, v := range payload {
				m[k] = v
			}
			return m
		}()),
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID}
			for k, v := range payload {
				m[k] = v
			}
			return m
		}()),
	)
	return events
}

// MarketExpert 专家市场的单个专家（/portal/operation-platform/market/expert/list 响应子集）。
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// MarketExpertList 拉取专家市场真实专家列表（expertType: "agent" 单专家 / "team" 专家团）。
// expert_actual_use 的判据校验要求 id 是平台上真实存在的专家（编造 id 不计数）。
func (c *Client) MarketExpertList(a *auth.Auth, expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	return c.marketExpertList(a, body)
}

// MarketExpertSearch 按关键词检索专家市场（等价于客户端「专家页搜索框」）。
//
// 请求体里的关键词字段名是 keyword（实测：keyword 生效时 total_count 收窄到 220，
// search_keyword / query / search 均被忽略返回全量 25318）。同一关键词可能命中多个
// 同名专家（display_name_zh 与 profession_zh 都会参与匹配），调用方必须按 expert_id
// 二次筛选，不能取首条。
func (c *Client) MarketExpertSearch(a *auth.Auth, keyword string) ([]MarketExpert, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, fmt.Errorf("market expert search: keyword required")
	}
	return c.marketExpertList(a, map[string]any{"page": 1, "page_size": 20, "keyword": keyword})
}

// marketExpertList 专家市场列表/搜索的公共请求（POST .../market/expert/list）。
func (c *Client) marketExpertList(a *auth.Auth, body map[string]any) ([]MarketExpert, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", desktopUAFor(a))
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	data, err := c.doJSON(req, a)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// DesktopChatWithExpert 发一条真实桌面指纹 chat 请求（可带 X-Expert-Id），从 SSE 流
// 解析**服务端返回的 requestId**（data.id，如 cmb-xxxx / 32hex）并返回。
// expert_actual_use 等 JOIN 事件的 requestId 必须是该服务端 id——自造 UUID 不计数
// （客户端 resolveRealRequestId 同款语义，Sunny row 2113 实证）。
func (c *Client) DesktopChatWithExpert(a *auth.Auth, expertID string) (conversationID, requestID string, err error) {
	// 会话 ID 用官方形态（v4 UUID）。旧实现是 `wb2api-conv-<UnixNano>`——项目缩写 +
	// 纳秒时间戳直接当 X-Conversation-ID 打给上游，是 issue #51 里最直白的一处自证。
	conversationID = session.NewConversationID()
	body := map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	// 头族一律走 ChatHeaders：这条路径原先手搓了 20 多行头，结果与主 chat 路径长期漂移
	// （缺 X-Trace-ID / X-B3-* / x-stainless-* / acp-connection-id，X-IDE-Version 写死
	// 5.5.6，X-Request-ID 还是十进制纳秒）。手搓头本身就是漂移的根因——统一出口后
	// 主路径改一次这里自动跟随。
	meta := ChatMeta{
		ConversationID:        conversationID,
		CacheKeyID:            conversationID,
		ConversationRequestID: session.NewMessageID(),
	}
	c.ChatHeaders(req, a, "", meta)
	if expertID != "" {
		req.Header.Set("X-Expert-Id", expertID)
	}
	if os.Getenv("WB2A_DEBUG_CHAT") != "" {
		fmt.Printf("[dbg] URL=%s\n", req.URL)
		for k := range req.Header {
			fmt.Printf("[dbg] %s: %s\n", k, req.Header.Get(k))
		}
	}
	resp, err := c.do(req, a, false)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if os.Getenv("WB2A_DEBUG_CHAT") != "" {
		fmt.Printf("[dbg] status=%d enc=%q\n", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("chat http %d: %s", resp.StatusCode, b)
	}
	// 从 SSE 流抓第一个 data.id 作为服务端 requestId（读干流避免残留连接）。
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 8192)
	searchFrom := 0
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if os.Getenv("WB2A_DEBUG_CHAT") != "" {
				dbg := string(tmp[:n])
				if len(dbg) > 120 {
					dbg = dbg[:120]
				}
				fmt.Printf("[dbg-rd %d] %q\n", n, dbg)
			}
			// 从上次搜索位置继续：SSE 里 `"id":"` 可能先出现在消息 id 等字段，
			// 若不推进偏移，首个不匹配的 id 会让循环永远重复命中同一位置，
			// 读满 1MB 后误报"未找到 requestId"。
			if i := bytes.Index(buf[searchFrom:], []byte(`"id":"`)); i >= 0 {
				abs := searchFrom + i
				rest := buf[abs+6:]
				if end := bytes.IndexByte(rest, '"'); end > 0 {
					id := string(rest[:end])
					if os.Getenv("WB2A_DEBUG_CHAT") != "" {
						fmt.Printf("[dbg-id] %q match=%v\n", id, idRegex.MatchString(id))
					}
					if idRegex.MatchString(id) {
						return conversationID, id, nil
					}
					searchFrom = abs + 1
				}
			}
		}
		if rerr != nil || len(buf) > 1<<20 {
			break
		}
	}
	return "", "", fmt.Errorf("SSE 中未找到服务端 requestId")
}

// idRegex 服务端 requestId 形状（cmb- 前缀 32hex 或裸 32hex）。
var idRegex = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// DesktopExpertSummonSequence 构造「召唤平台专家」事件组（expert_summon_click 等），
// 载荷对齐真实抓包样本（Sunny row 2644）。需配合 DesktopChatWithExpert +
// DesktopExpertActualUseEvent 完成一次完整「召唤+使用」。
func DesktopExpertSummonSequence(e MarketExpert) []DesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return []DesktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 构造「专家真实使用」事件（expert_5/Expert_team_use_3 计数）。
// requestID 必须是 DesktopChatWithExpert 返回的服务端 requestId。
func DesktopExpertActualUseEvent(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"
	return ev
}

// DesktopExpertActualUseLocal mode:"LOCAL" 变体（Expert_lighthouse 判据要求 LOCAL，
// 对齐真实样本 Sunny row 868：轻量云专家使用时 mode=LOCAL、type 为空、cost=0）。
func DesktopExpertActualUseLocal(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "LOCAL"
	return ev
}

// desktopExpertActualUse expert_actual_use 公共载荷。
func desktopExpertActualUse(e MarketExpert, conversationID, requestID string) DesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return DesktopEvent{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.DisplayNameZH, "expertTitle": e.ProfessionZH,
		"type": cat, "expertType": e.ExpertType, "source": "builtin", "version": ver,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + requestID[len(requestID)-8:],
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}
