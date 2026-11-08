// conv_fields.go 把会话轮级字段写进出站消息体，对齐官方桌面端的 chat body 形态
// （issue #51 body 侧）。
//
// 官方实测（国际版 5.8.4 / 国内版 5.8.3 同构，见 capture/raw3、capture/raw8）：
//
//	messages[i] = {
//	  "role": "user",
//	  "content": [{"type":"text","text":"..."}],
//	  "agent": "cli",
//	  "conversationRequestId": "<该轮的 X-Conversation-Request-ID>",   ← 轮级
//	  "startsNewUserRequest": true,
//	}
//	messages[i] = {          // assistant
//	  "role": "assistant", ..., "conversationRequestId": "<产出它的那一轮>",
//	  "messageId": "<那条消息的 X-Request-ID>", "traceId": ...,
//	  "model"/"requestModelId"/"requestModelName"/"usage"/"rawUsage"/"reasoning"/"stepSeq"
//	}
//
// 也就是说：客户端把「这一轮属于哪个 request-id」同时写在**头**和**体**里。
// 网关之前只写了头，body 里一条都没有——头体不一致本身就是可判别的形态。
//
// 边界（不伪造）：
//   - 只给「当前轮」的最后一条 user 消息写 conversationRequestId；
//   - 历史消息的轮级 ID 客户端没给（OpenAI 协议无此字段），网关无从得知，**不编造**——
//     宁可少写，也不写一个对不上的值（对不上的 ID 比缺失更容易被关联分析抓出来）；
//   - assistant 消息的 messageId / usage / model 等由真实调用产出，历史部分同样不补。
package upstream

import (
	"encoding/json"
	"strings"
)

// agentCLI 官方 chat 出站每条消息都带的 agent 段（两版实测均为 `cli`）。
const agentCLI = "cli"

// injectConversationFields 在已改写的出站 body 上补会话轮级字段（字节入口）。
//
// 热路径不走这里：prepareBodyOpt 已经持有解析好的 obj，注入与它的序列化合并成
// 一次（见 injectConversationFieldsObj）。本函数保留给测试与独立调用方。
func injectConversationFields(body []byte, convReqID string) []byte {
	if len(body) == 0 || convReqID == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if !injectConversationFieldsObj(obj, convReqID) {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// injectConversationFieldsObj 在已解析的 obj 上补会话轮级字段，返回是否改动。
// convReqID 为空或没有可写的当前轮时不改动（不写空值、不凭空造消息）。
func injectConversationFieldsObj(obj map[string]any, convReqID string) bool {
	if obj == nil || convReqID == "" {
		return false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	// 找最后一条 user 消息：它就是「当前轮」的起点（官方 startsNewUserRequest 语义）。
	idx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); strings.EqualFold(role, "user") {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	cur, ok := msgs[idx].(map[string]any)
	if !ok {
		return false
	}
	// 客户端若已自带（OpenAI 兼容网关转发官方 body 的场景）→ 原值优先，不覆盖。
	if v, _ := cur["conversationRequestId"].(string); v != "" {
		return false
	}
	cur["conversationRequestId"] = convReqID
	cur["startsNewUserRequest"] = true
	if v, _ := cur["agent"].(string); v == "" {
		cur["agent"] = agentCLI
	}
	msgs[idx] = cur
	obj["messages"] = msgs
	return true
}
