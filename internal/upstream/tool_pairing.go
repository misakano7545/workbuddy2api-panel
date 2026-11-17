// tool_pairing.go 出站请求体的孤儿 tool_call↔tool 配对清理 + tool 结果块重排
// （吸收参考仓库 sse.ts:91-123 resolveToolPairing 语义，适配网关的 OpenAI wire 消息形态）。
//
// 背景：OpenAI 兼容协议要求带 tool_calls 的 assistant 消息，其每一个 tool_call id
// 都必须有对应的一条 role:tool 结果消息；反之 role:tool 消息也必须有对应的前置
// tool_call。缺任一侧，上游都会以 HTTP 400 拒绝整个请求。
//
// 工具执行失败时（参数非法、超时、工具不存在……）客户端会把 assistant 的 tool_calls
// 持久化进会话历史，却写不回结果消息。这条坏历史随后被每次请求原样重放——上游对之后
// 每一条用户消息都返回 400，整条会话报废。网关是最后一道防线：发出请求前剔除无法配对
// 的条目让会话自愈，宁可丢一轮工具上下文，也好过整条会话死亡。
package upstream

import "strings"

// repackToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的非 tool 消息
// 挪到整组之后，保证同一批 tool_call 的结果在 wire 上连续。
//
// 背景：Codex 的 image_resize_notice 特性会把 <image_resize_notice> 作为一条
// developer/system 消息插在 tool 输出后面。并行调用时它插在两份 tool 结果中间：
//
//	assistant tool_calls=[c00 c01]
//	tool c00
//	developer <image_resize_notice>   <- 插在中间
//	tool c01
//
// OpenAI 兼容协议要求 tool 结果紧跟 assistant，中间插任何消息都算配对断裂，上游判
// 11148（tool_call_sequence_broken）并顶死整条会话。这里只调顺序、不改内容：
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 结果顺序保持不变（同批 tool_call 的原相对顺序 = 结果顺序），不引入新的顺序敏感
// 问题。无插入消息时零改动零分配（返回原 slice）。
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		// 收集紧随其后（允许被其他消息打断）的同批 tool 结果，按原相对顺序。
		out = append(out, messages[i])
		i++
		var results []any
		var between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // assistant 后没有结果：交由 cleanupOrphanToolCalls 处理
			}
			// 下一组 assistant.tool_calls 是新的组头，绝不能当插入物吞掉：一旦被收进
			// between，它永远不再被外层循环当作组头处理，它自己那批结果也就永远得不
			// 到重排（真实会话 msg[181] 正是这样漏掉的）。必须 break 交还外层循环。
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			// 同批结果尚未收齐时，中间消息视为插入物，暂存待后移。
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// reorderEarlyToolResults 把「早于其 tool_call」出现的 role:tool 结果移到对应
// assistant(tool_calls) 之后，保证 wire 上永远是「调用在前、结果在后」。
//
// 背景：OpenAI 兼容协议按位置配对——tool 结果必须出现在声明它的 assistant 之后。
// 有些客户端会把这个顺序写反（Codex 经 cc-switch 类本地代理转发 /v1/responses 时，
// input 里 function_call_output 排在 function_call 之前；本仓 responses 适配器的
// convertInputItems 按原始顺序拼接，于是出站就是 tool(c0) 在前、assistant(tool_calls
// c0) 在后），上游判 11148 tool_call_sequence_broken 顶死整条会话。
//
// 参照 hub PR #3（PolarisTime，niyouqianma/workbuddy2api-hub）的
// reorder_early_tool_results：只调顺序、不改内容，工具语义与结果正文都原样保留。
//
// 目标位置是所属 assistant 之后、**其后连续 tool 结果之后**（与 PR 同口径），
// 因此用「延迟到结果区结束再落地」而不是插在 assistant 紧跟的位置：正常顺序的
// 结果先出，早到的那几条补在同批末尾，同批内的原相对顺序保持不变。
//
// 归属判定：**id 会跨轮复用**——上游没给 id 时网关自己按轮内下标补 call_0 / toolu_0
// （responses.go / messages.go 的 openTool），同一会话里每轮都可能有一个 call_0。
// 所以不能「同 id 跟最后一次声明走」（PR 的口径）：那会把顺序本来正确的第一轮结果
// 搬到最后一轮之后，反而制造 11148。这里正序扫描，记每个 id「已声明、未应答」的次数：
//   - 结果命中未应答的声明 → 正常配对，原地不动；
//   - 否则它不属于前面任何一次声明：后面有同 id 的声明就是早到结果，归**最近**那一次，
//     并预先记作该次已应答（之后同 id 的结果不会再被它吸走）；
//   - 前后都找不到声明者的孤儿结果不动，交给 cleanupOrphanToolCalls 删；
//   - 本来顺序就对的请求零改动零分配（返回原 slice）。
func reorderEarlyToolResults(messages []any) ([]any, bool) {
	if len(messages) < 2 {
		return messages, false
	}
	// 每个 id 的全部声明下标（正序遍历 → 升序）。
	decls := map[string][]int{}
	for i, raw := range messages {
		for _, id := range declaredToolCallIDs(raw) {
			decls[id] = append(decls[id], i)
		}
	}
	if len(decls) == 0 {
		return messages, false
	}
	open := map[string]int{} // id → 已声明未应答次数；早到结果先扣，可暂为负
	group := map[int][]any{} // 声明下标 → 归它的早到结果（原相对顺序）
	early := make([]bool, len(messages))
	anyEarly := false
	for i, raw := range messages {
		for _, id := range declaredToolCallIDs(raw) {
			open[id]++
		}
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "tool" {
			continue
		}
		id, _ := m["tool_call_id"].(string)
		if open[id] > 0 {
			open[id]--
			continue
		}
		for _, j := range decls[id] {
			if j > i {
				early[i] = true
				anyEarly = true
				group[j] = append(group[j], raw)
				open[id]--
				break
			}
		}
	}
	if !anyEarly {
		return messages, false
	}

	out := make([]any, 0, len(messages))
	var pending []any
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, pending...)
		pending = nil
	}
	for i, raw := range messages {
		if early[i] {
			continue // 早到的结果不在此处落地
		}
		role := ""
		if m, ok := raw.(map[string]any); ok {
			role, _ = m["role"].(string)
		}
		// 结果区结束（来了一条非 tool 消息）：先把上一批待补的结果补上。
		if len(pending) > 0 && role != "tool" {
			flush()
		}
		out = append(out, raw)
		if g, ok := group[i]; ok {
			pending = g
		}
	}
	flush()
	return out, true
}

// declaredToolCallIDs 返回一条 assistant 消息 tool_calls 里的非空 id（其他消息为 nil）。
func declaredToolCallIDs(raw any) []string {
	m, ok := raw.(map[string]any)
	if !ok || m["role"] != "assistant" {
		return nil
	}
	tcs, _ := m["tool_calls"].([]any)
	var ids []string
	for _, tci := range tcs {
		if tc, ok := tci.(map[string]any); ok {
			if id, _ := tc["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// cleanupOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果（所有模型，独立于
// deepseek-only 的 sanitize 开关）。语义对齐参考仓库 resolveToolPairing：
//
//   - 收集全线 role:tool 消息的 tool_call_id（结果集）与 assistant.tool_calls[].id（调用集）；
//   - 一批 assistant.tool_calls 按 keepCalls 对称裁剪：只留有结果配对的调用（部分保留
//     不会留下无结果的 tool_call），过滤后为空才删掉整个 tool_calls 键；
//   - role:tool 只在对应 tool_call 被保留时才保留，否则删除整条消息；
//   - 无任何工具流量 → 原 slice 原样返回，changed=false（零分配零改动）。
//
// 这是「让请求通过」的安全网：只要存在合法配对就整段保留这些字段，绝不吞掉正确配对。
// 返回清理后的 slice（无改动时等于原 slice，勿依赖其是否新分配）及是否发生删除。
func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	nameBadIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						callIDs[id] = true
						if toolCallNameMissing(tc) {
							nameBadIDs[id] = true
						}
						hasTraffic = true
					}
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	// keepCalls：调用 id 是否双侧齐全（调用存在且结果存在）。重复 id 与乱序均按集合处理。
	//
	// 追加否决条件：没有可用功能名（无 function 键 / 无 name 键 / name 空白）的调用一律
	// 不保留。上游对这类条目直接判 11133 model_param_invalid（tool_calls[i].function.name
	// must be a non-empty string），**带不带配对结果都拒**——带结果的脏条目原本会命中
	// keepCalls 原样出站，正是线上「整条会话每轮 400、只能手动清空历史」的成因。删除动作
	// 完全复用两侧对称裁剪（调用与其结果成对剔除），配对对称不变式照常成立。
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if nameBadIDs[id] {
			continue
		}
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) assistant.tool_calls：按 keepCalls 对称裁剪——只留有结果的调用，过滤后为空则删键。
	//
	// 历史实现是「批内每个 id 都齐才整批保留，否则删掉整个 tool_calls 键」。那会留下
	// 无主结果：批 [c1,c2] 只回了 c1 时，调用侧整批被删，而 tool{c1} 仍按 id 命中
	// keepCalls 得以保留 —— 出站载荷于是变成「无 tool_calls 的 assistant + 孤儿 tool」，
	// 上游判 11148（tool calls and tool results do not match）并顶死整条会话。
	// 现在两侧共用同一份 keepCalls 按 id 对称裁剪（与 2) 的删除侧同口径），
	// 任何输入都不会再产生半截配对。
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		keptCalls := make([]any, 0, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := tc["id"].(string); keepCalls[id] {
				keptCalls = append(keptCalls, tc)
			}
		}
		if len(keptCalls) == len(tcs) {
			continue // 整批齐全：零改动
		}
		changed = true
		if len(keptCalls) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = keptCalls
	}
	// 2) role:tool 结果：只有对应 tool_call 被保留才保留；孤儿结果整条删除。
	kept := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				continue
			}
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}

// toolCallNameMissing 报告 assistant.tool_calls[] 单项是否缺少可用的功能名：
// function 对象缺失、name 字段缺失、name 为空白串，三者皆判真。
//
// 为什么比「function 在、name 键存在且为空」宽一格：本网关自己就能产出另两种形状——
// 流在 name 分片到达前断掉时，聚合器落下的占位条目是 `{"id":..,"type":"function"}`
// （无 function 键）或 `{"id":..,"function":{}}`（有 function 无 name 键），实测见
// Aggregate。这类条目客户端无从派发、上游无从校验，留着只会把整条会话顶死。
// OpenAI schema 本就要求 function.name 非空，故不存在「合法但缺名」的入站历史，
// 判脏不会误伤正确配对——代价只是丢一轮工具上下文，正是本文件的既定取舍。
func toolCallNameMissing(tc map[string]any) bool {
	fn, ok := tc["function"].(map[string]any)
	if !ok {
		return true
	}
	name, ok := fn["name"].(string)
	if !ok {
		return true
	}
	return strings.TrimSpace(name) == ""
}
