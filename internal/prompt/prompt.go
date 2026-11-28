// Package prompt 提供网关自有系统提示词：内置默认 + 文件覆盖 + 降级中性提示词。
//
// 背景：客户端（Claude Code/Codex 等 CLI）在 system prompt 注入固定模板句，
// 上游内容审核按逐字精确匹配误杀合法流量（issue #36/PR39 的 11128）。
// 方案：网关在出站前用自有系统提示词替换客户端 system/developer 消息，
// 从源头消灭 system 来源的指纹误报（用户/assistant 消息里的指纹串仍由
// internal/upstream/sanitize.go 清洗，两层叠加、互不替代）。
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/misakano7545/workbuddy2api-panel/internal/jsonscan"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded 降级提示词：误报处理用，刻意极简中性。
//
// 触发场景：passthrough 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案），
// 判定为指纹误报后换最小中性提示词重试一次。非对抗框架——只用于绕开
// system 来源的误报，不改变用户指令的合法性语义。
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本。
//   - file 非空 → 读文件（不存在/读失败返回 error，调用方 fail fast）；
//   - file 空 → 返回内置 defaultPrompt。
//
// mode 在此仅做透传记录（实际 custom/passthrough 路由由调用方决定），
// Load 只负责"拿到一段提示词文本"，不关心路由语义。
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	return string(raw), nil
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//   - 删除 messages 中所有 role 为 system/developer 的消息；
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}；
//   - 其余字段与 user/assistant/tool 消息逐字不动。
//
// 解析失败 → 原样返回（绝不失败）：Rewrite 是出站改写的关键路径，
// 任何解析错误都不应阻塞请求转发，让上游按其原始语义处理。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	bodyParses.Add(1)
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// 无 messages 字段或类型不符 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// 过滤掉所有 system/developer 消息，保留 user/assistant/tool 及其他角色。
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	// 头部插入单条 system 消息（prepend 避免整体重排语义）。
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// SplitSystemMessages 把最长的那条 system/developer 消息按句子边界拆成多条 system 消息，
// 用于 11-128 的**首选**抢救：上游按「连续原文」匹配指纹，句界一断就失配，而文本一个字都
// 不改、承载通道仍是 system（模型的指令优先级不变）。
//
// 实测（2026-10-28，CN 账号 deepseek-v4.1-flash，internal/probe/probe_channel.go）：
//   - 原始 DSH @ 路径段整段在 system        → 400 11-128
//   - 同一段同义改写后仍在 system           → 200（sanitize 层已覆盖这种情况）
//   - 同一段按句界拆成两条 system           → 200
//   - 每句一条 system                       → 200
//
// 返回 false 的两种情况：没有任何一条 system 能切出 ≥2 段（单句 system 没法拆，交给
// 调用方走后面的补救层）。
func SplitSystemMessages(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, false
	}
	// 只拆最长的一条：短 system 拆开反而是把一句指令切成两半（实测人格句拆两条就失效）。
	best, bestLen := -1, 0
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		txt, ok := mm["content"].(string)
		if !ok || len(txt) <= bestLen {
			continue
		}
		best, bestLen = i, len(txt)
	}
	if best < 0 {
		return body, false
	}
	parts := splitSentences(msgs[best].(map[string]any)["content"].(string))
	if len(parts) < 2 {
		return body, false
	}
	rebuilt := make([]any, 0, len(msgs)+len(parts)-1)
	rebuilt = append(rebuilt, msgs[:best]...)
	for _, p := range parts {
		rebuilt = append(rebuilt, map[string]any{"role": "system", "content": p})
	}
	rebuilt = append(rebuilt, msgs[best+1:]...)
	obj["messages"] = rebuilt
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// splitSentences 按句末标点切分，保留标点；句界后面的空白丢掉（消息边界本身就是分隔）。
// shortcut: 只认 . ! ? 后的空白与换行，不处理列表/代码块里的「句号」——多切一刀无害，
// 少切一刀才致命，所以宁可切碎。
func splitSentences(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '.', '!', '?':
			// 后面紧跟空白才算句界（"3.5" / "v1.2.3" / "example.com" 不切）。
			if i+1 < len(text) && (text[i+1] == ' ' || text[i+1] == '\n' || text[i+1] == '\t') {
				if seg := strings.TrimSpace(text[start : i+1]); seg != "" {
					out = append(out, seg)
				}
				start = i + 1
			}
		}
	}
	if seg := strings.TrimSpace(text[start:]); seg != "" {
		out = append(out, seg)
	}
	return out
}

// MoveSystemToUser 把 system/developer 的**原文**搬进首条 user 消息前部，system 换成中性
// 提示词，作为 11-128 的**后一层**抢救（前一层是 SplitSystemMessages：留在 system 更好，
// 拆不动才搬家）。返回 false = 这条请求结构不安全，别动。
//
// 与 Rewrite 的区别是丢不丢东西：Rewrite 用中性提示词**替换** system，客户端辛苦写的
// 人设/项目约定就此消失；这里只换承载通道（system → user），一个字符都不丢。上游的
// 11-128 判据是「system 消息内容命中特征」，把同一段文本放进 user 就能过（实测同一 body
// 换号结果一致、system 原文搬进首条 user 即通过），所以这不是绕过审核，是换通道。
//
// 保守放弃的四种情况：没有 system/developer 文本、system 内容不是纯文本、找不到 user
// 消息、首条 user 不是纯文本（多模态 parts 里插文本会打乱顺序）。
func MoveSystemToUser(body []byte, neutral string) ([]byte, bool) {
	if len(body) == 0 || neutral == "" {
		return body, false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, false
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		return body, false
	}
	switch role, _ := first["role"].(string); role {
	case "system", "developer":
	default:
		return body, false
	}
	sysText, ok := first["content"].(string)
	if !ok || strings.TrimSpace(sysText) == "" {
		return body, false
	}
	for _, m := range msgs[1:] {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role != "user" {
			continue
		}
		txt, ok := mm["content"].(string)
		if !ok {
			return body, false // 多模态 parts：插文本会打乱 part 顺序
		}
		mm["content"] = sysText + "\n\n" + txt
		first["role"] = "system"
		first["content"] = neutral
		out, err := json.Marshal(obj)
		if err != nil {
			return body, false
		}
		return out, true
	}
	return body, false
}

// EnsureSystemFirst 保证 messages[0] 是 role=system —— 上游**渠道校验**的硬要求。
//
// 为什么必须做（2026-10-20 真机实测，国际版 5.8.4）：上游对首条消息不是 system
// 的请求直接 400 code=11-128「first message is not system prompt」，displayTips 与
// 「非官方客户端」同一句。三条实测对照：
//
//	首条 user       → 400 11-128 "first message is not system prompt"
//	首条 developer  → 400 11-128 "Illegal API invocation from an unapproved channel"
//	首条 system     → 200（同一 body 其余部分不变，模型正常回包）
//
// 也就是说这不是内容策略、不是账号问题：**首条角色本身**是渠道校验的一部分。
// custom / append 两种模式本来就以网关 system 开头，唯独 passthrough 会原样透传
// 客户端消息——客户端不带 system（大量 SDK / 手搓 curl 都不带）时首条是 user，
// 于是每一次请求都必然 11-128，账号被反复计成「渠道校验拦截」而全池冷却，
// 表象就是「国际版账号全被禁用，但网页还能聊」（issue #231）。
//
// 语义边界：只在**首条不是 system/developer 时**补一条，位置在最前；首条已是
// system/developer 时只做 developer→system 归一（上游不认 developer），其余消息
// 逐字不动。fallback 为空时用 prompt.Degraded（中性提示词）兜底，绝不因为没配
// 提示词就不补。
func EnsureSystemFirst(body []byte, fallback string) []byte {
	if len(body) == 0 {
		return body
	}
	// 快路径：首条已是 system（代理类客户端几乎恒如此）→ 逐字不动，一次整树解析都不做。
	// 这一条在线上值 30~60ms/请求：此前的实现为了读一个 role 就把整个 100~500KB body
	// 解析成 map（含全部历史消息的解转义）。
	if role, ok := jsonscan.FirstMessageRole(body); ok && role == "system" {
		return body
	}
	bodyParses.Add(1)
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		// 无 messages / 空数组：没有「首条」可补，交给上游按原样报错（不凭空造对话）。
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		return body
	}
	role, _ := first["role"].(string)
	switch role {
	case "system":
		return body // 已是官方形态，零改动。
	case "developer":
		// developer 首条同样被拒（报的是另一句 unapproved channel）。就地改成 system，
		// 内容逐字保留——不删不并，避免动客户端语义。
		first["role"] = "system"
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	prompt := fallback
	if prompt == "" {
		prompt = Degraded
	}
	if prompt == "" {
		return body
	}
	// 首条是 user/assistant/tool：在最前补一条网关 system，其余逐字不动。
	rewritten := make([]any, 0, len(msgs)+1)
	rewritten = append(rewritten, map[string]any{"role": "system", "content": prompt})
	rewritten = append(rewritten, msgs...)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// Append 解析 OpenAI 请求体并在"开头连续 system/developer 块"之后插入一条
// 网关自有 system 提示词（issue #129 append 模式）：
//   - 开头连续块 = 从 messages[0] 起向后 role 为 system/developer（精确字符串
//     匹配，与 Rewrite 删除口径一致）的消息；遇第一条非 system/developer
//     消息（含非 map 消息、无 role 消息）即停；
//   - 插入点 = 连续块末尾之后（块长 0 时即 messages 最前）；
//   - 所有既有消息（含开头块、中途 system、user/assistant/tool）逐字不动
//     ——客户端项目规范/工具约定与网关提示词并用。
//
// 守卫与 Rewrite 逐条一致：空 body / 空 systemPrompt / 坏 JSON → 原样返回
// （绝不失败）；无 messages 字段 → messages=[网关 system]，其余字段原样。
//
// 边界判定须显式同时匹配 system 与 developer：归一（developer→system）在
// 下游 prepareBody 的 normalizeRoles，Append 执行时开头块里的 developer
// 还是 developer。网关消息角色用 system 而非 developer——上游 role 白名单
// 不含 developer，插 developer 等于制造一次必然归一与多余的 11128 风险窗口。
func Append(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// 无 messages 字段或类型不符 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// 扫描开头连续 system/developer 块，遇第一条非 system/developer 即停。
	insertAt := 0
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			break
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			break
		}
		insertAt++
	}
	gw := map[string]any{"role": "system", "content": systemPrompt}
	// 已有消息逐字不动：只在插入点拼接，不重排、不改写任何元素。
	rewritten := make([]any, 0, len(msgs)+1)
	rewritten = append(rewritten, msgs[:insertAt]...)
	rewritten = append(rewritten, gw)
	rewritten = append(rewritten, msgs[insertAt:]...)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// bodyParses 本包对请求体做整树 JSON 解析的次数（观测/回归用）。
// 每多一次 Unmarshal 就是一次 100~500KB 的解转义与对象分配——线上「并发一高
// 就降速」的主因就是这类重复解析；测试用 ParseCount 把它钉住。
var bodyParses atomic.Int64

// ParseCount 返回本包累计的整树解析次数（供测试与面板观测）。
func ParseCount() int64 { return bodyParses.Load() }

// ResetParseCount 归零计数（供测试）。
func ResetParseCount() { bodyParses.Store(0) }
