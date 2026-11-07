// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. image_url 归一化（上游只认 OpenAI 对象形态，字符串会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"math"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上把 reasoning_effort 对齐到模型支持的档位：
// 仅当请求显式携带且模型不支持该档位时，改为**最近**的支持档（同距优先模型默认档，其次偏高）；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不改写）。
//
// 向后兼容封装：不传 defaultEfforts（无模型声明默认档），thinking.go 回退硬编码 high。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault 完整管线：efforts 降级 + thinking.go 按
// defaultEfforts（模型声明默认档）补档。defaultEfforts 为 nil 时与旧行为一致
// （deepseek 缺档回退硬编码 high）。
func PrepareBodyOptWithEffortsAndDefault(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	body, _ := PrepareBodyOptWithEffortsDefaultAndCaps(src, sanitize, efforts, defaultEfforts, nil)
	return body
}

// SplitRealmPrefix 解析网关的 realm 前缀协议（"[realm:]model"）：
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm 回落 "cn"。
// 大小写敏感（前缀必须是精确的小写枚举）。server.resolveModel 委托本函数，
// 保证选号、粘性、出站重写三处口径一致。
func SplitRealmPrefix(model string) (realm, bare string) {
	if i := strings.IndexByte(model, ':'); i >= 0 {
		switch model[:i] {
		case "cn", "global":
			return model[:i], model[i+1:]
		}
	}
	return "cn", model
}

// normalizeModelName 把出站 model 剥成裸名：上游只认裸名，realm 前缀是网关侧
// 路由协议。model 缺失或非字符串时保持原样（不二次错误化坏 body）。
func normalizeModelName(obj map[string]any) {
	cur, ok := obj["model"].(string)
	if !ok {
		return
	}
	if _, bare := SplitRealmPrefix(cur); bare != cur {
		obj["model"] = bare
	}
}

// PrepareBodyOptWithEffortsDefaultAndCaps 在 …AndDefault 之上接入模型输出上限快照：
// 无显式限额的请求按目录动态值补 max_tokens（见 injectCatalogMaxTokens）。
// maxOut 为 nil 时行为与 …AndDefault 完全一致。
// 返回改写后的请求体与本次**实际生效**的思考档位（未携带该字段/未知模型/未知档位 → ""）。
//
// 导出入口不注入 prompt_cache_key（保持既有公开语义）；生产路径由
// client.prepareBody 调用 prepareBodyOpt 并传入 cache 参数。
func PrepareBodyOptWithEffortsDefaultAndCaps(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string, maxOut map[string]int64) ([]byte, string) {
	body, _, eff := prepareBodyOpt(src, sanitize, true, efforts, defaultEfforts, maxOut, "", nil, "", "", false, 0)
	return body, eff
}

// PrepareBodyOptWithEffortPolicy 在 …Caps 之上接入网关思考档位策略（config
// thinking 节）：客户端缺档补网关默认档；client_priority=false 时按网关档位
// 改写客户端显式档（override_scope 控制是否连「关闭思考」一起改写）。
// realm 用于 thinking.model_defaults 的 "realm:model" 精确匹配；policy 为 nil
// 或未启用时与 …Caps 完全一致。
func PrepareBodyOptWithEffortPolicy(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string, maxOut map[string]int64, realm string, policy *EffortPolicy) ([]byte, string) {
	body, _, eff := prepareBodyOpt(src, sanitize, true, efforts, defaultEfforts, maxOut, realm, policy, "", "", false, 0)
	return body, eff
}

// prepareBodyOpt 是完整管线：injectCache=true 时在同一次 map 解析里注入
// prompt_cache_key（优先级见 promptCacheKeyFor），避免二次 Unmarshal/Marshal。
//
// 返回 (改写后请求体, 客户端原始请求档位, 本次实际生效档位)：
//   - requested 在 injectThinking 之前抓取，因此不含网关补的默认档——它回答
//     「客户端到底发没发、发的哪一档」（issue #43/#18 的判别依据）；
//   - effective 是降级后的出站档位，未携带/未知模型/未知档位 → ""。
func prepareBodyOpt(src []byte, sanitize, brandGlobal bool, efforts map[string][]string, defaultEfforts map[string]string, maxOut map[string]int64, realm string, policy *EffortPolicy, cacheUID, cacheConversation string, injectCache bool, keepRounds int) ([]byte, string, string) {
	if len(src) == 0 {
		return src, "", ""
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, "", ""
	}
	// 先剥 realm 前缀：后续 injectCatalogMaxTokens / clampGPTMinMaxTokens /
	// injectThinking / normalizeReasoningEffort 都按裸模型名查表，晚一步就会查空。
	normalizeModelName(obj)
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（吸收上游 PR #116，Closes #117）：
	// OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新字段；
	// DeepSeek Harness 等新客户端只发别名。WorkBuddy 上游（CN /v2 与 global
	// /console 同源）只认 max_tokens——别名透传会被上游忽略后回落默认输出上限
	// （实测 32000），长流任务被截。
	translateMaxCompletionTokens(obj)
	// GPT 系过小的 max_tokens 抬到上游下限（吸收上游 fd3e142）：
	// Claude Code 切模型时发的探针 max_tokens 极小，上游 GPT 系对 <16 一律
	// 400 code=11133，全号轮转同样被拒 → 客户端永远切不过去。
	clampGPTMinMaxTokens(obj)
	// 目录动态值补默认输出上限（同 PR #116 的另一个缺口：多数客户端连别名都
	// 不发——Codex 实测不带任何限额字段，被上游默认 32000 截断）。
	injectCatalogMaxTokens(obj, maxOut)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeToolPatterns(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	normalizeStop(obj)
	dropReasoningContentParts(obj)
	// 历史裁剪先于 tool 配对：cut 落在整轮边界上，不会切出半个 tool 配对；
	// 万一客户端自己的历史本来就断了，后面两步照样兜底。
	trimHistory(obj, keepRounds)
	// tool 配对两步（见 tool_pairing.go）：先重排再清理。所有模型一律执行（独立于
	// deepseek-only 的 sanitize 开关）。这是「让请求通过」的安全网——不完整配对的
	// tool_calls/tool 结果会让上游对之后每条消息都返 400，必须先行剔除；
	// 插在结果中间的非 tool 消息（Codex image_resize_notice）同样判配对断裂，
	// 先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// 无改动时两步都返回原 slice，这里回写等于零操作；任一步重排/删除
		// （哪怕后续步骤零改动）也必须落到 obj——不能只在「最后一步改动」时回写，
		// 否则 repack 单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
	}
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// requested 必须在 injectThinking 之前抓：它要回答的是「客户端发了什么」，
	// 而 injectThinking 会给缺档的请求补默认档——补完之后两者就再也分不开了
	// （issue #43：客户端设 max、日志显示 high，读者无法判断是「没收到 max」
	// 还是「收到了但降级」，这一列必须能自证）。
	requested := requestedEffort(obj)
	modelName, _ := obj["model"].(string)
	// 网关思考档位策略（见 effort_policy.go）同样先于 injectThinking：
	// override_scope=all 要把客户端的 off/disabled 改写成「开启 + 目标档」，
	// 晚一步就会被 injectThinking 写成 thinking.type=disabled 并删掉档位。
	// client_priority=true 时这里只在「客户端没发档位」时补默认档。
	policyEffort := applyEffortPolicy(obj, modelName, efforts[modelName], policy, realm)
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	effort := normalizeReasoningEffort(obj, efforts, defaultEfforts)
	if policyEffort != "" {
		// 网关写入的目标档已是「解析到支持列表」的结果；降级管线不会再改它，
		// 但 effective 以实际写入值为准（off/disabled 被改写的场景 normalize
		// 也可能返回空）。
		effort = policyEffort
	}
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs, brandGlobal)
		}
		// tools 是客户端自带的能力声明，描述由客户端作者撰写，同样会带身份句
		// （上游 issue：只洗 messages 时整段工具描述原样出站）。issue #395 同类。
		sanitizeTools(obj["tools"], brandGlobal)
	}
	// prompt_cache_key：与 body 改写共用同一次 map，避免再 Unmarshal/Marshal 一遍。
	if injectCache {
		if key, ok := promptCacheKeyFor(obj, cacheUID, cacheConversation); ok {
			obj["prompt_cache_key"] = key
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src, requested, effort
	}
	return out, requested, effort
}

// trimHistory 只保留最近 keep 轮对话（issue #99）：一轮 = 一条 user 消息，连同其后
// 的 assistant/tool 回复。更早的整轮丢弃——长会话的输入 token 几乎全在这里。
//
// 三条约束：
//   - system 消息一条不删。它们是协议与人格，删了上游要么 400 11-128 要么人格丢失；
//     cut 之前出现的 system 也提到前面保留（客户端把 system 插在中间的情况）。
//   - cut 只落在 user 下标上，所以保留段开头永远不是 tool 结果——不会切出半个
//     tool 配对（配不成对的上游直接 400）。客户端自己断掉的历史仍由后面的
//     repack/cleanup 兜底。
//   - keep <= 0 时不动 body（默认关闭，与没有这个功能完全一致）。
func trimHistory(obj map[string]any, keep int) {
	if keep <= 0 {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	users := make([]int, 0, 8)
	for i, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "user" {
			users = append(users, i)
		}
	}
	if len(users) <= keep {
		return
	}
	cut := users[len(users)-keep]
	kept := make([]any, 0, len(msgs)-cut+4)
	for _, m := range msgs[:cut] {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "system" {
			kept = append(kept, m)
		}
	}
	obj["messages"] = append(kept, msgs[cut:]...)
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收上游 PR #116）。规则：显式 max_tokens 优先（别名只删）；
// 别名非正数值（0/null/负数）不翻译（0/null 语义是「未设置」，负数是非法值，
// 翻译等于把垃圾搬进 max_tokens）；非数值别名（字符串等畸形）不翻译（原样
// 透传由上游报 11101 参数错）。两域同口径：CN /v2 与 global /console 是同一套
// API，翻译不分 realm。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（减少 body 体积与排障噪音）
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免 1.28e5 科学计数法/小数
	// 尾巴进上游 body）；其他数值类型防御性兼容（int 家族——手构造 map 的调用方）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// gptMinMaxTokens GPT 系上游接受的 max_tokens 下限。
const gptMinMaxTokens = 16

// clampGPTMinMaxTokens 把 GPT 系模型过小的 max_tokens 抬到下限（吸收上游 fd3e142）。
//
// 背景：上游 GPT 系（实测 gpt-6-sol / gpt-6-luna / gpt-5.6-sol）对 max_tokens < 16
// 一律 400 code=11133 model_param_invalid（15 拒、16 过，同号同 body 对照）；hy4 等
// 非 GPT 模型无此限制。Claude Code 切模型时发 max_tokens 极小的探针，全号轮转同样
// 被拒 → 客户端 503，模型永远切不过去。账号与 body 其余部分无关，换号无用，只能
// 在发送前修。抬到下限只放宽输出上限、不改语义；未携带字段 / 非数值 / 已达下限一律
// 不动。判据用 id 含 "gpt-"（realm 前缀 global:gpt-… 同时命中）。
func clampGPTMinMaxTokens(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.Contains(strings.ToLower(model), "gpt-") {
		return
	}
	var v int64
	switch n := obj["max_tokens"].(type) {
	case float64:
		v = int64(n)
	case int64:
		v = n
	case int:
		v = int64(n)
	default:
		return
	}
	if v < gptMinMaxTokens {
		obj["max_tokens"] = int64(gptMinMaxTokens)
		log.Printf("max_tokens clamped model=%s %d -> %d", model, v, gptMinMaxTokens)
	}
}

// injectCatalogMaxTokens 无显式输出限额时，按模型目录动态声明的 maxOutputTokens
// 补 max_tokens。上游对缺失限额的请求套保守默认（实测 32000），而目录声明值才是
// 模型真实预算（如 cn:deepseek-v4.1-flash 393216）——Codex 这类不发限额字段的
// 客户端写大文件会在 32000 处被无声截断（长流任务被切）。显式限额（含别名翻译
// 产物）优先，不覆盖；模型未知/目录无值不补（不编造）。
func injectCatalogMaxTokens(obj map[string]any, maxOut map[string]int64) {
	if len(maxOut) == 0 {
		return
	}
	if _, has := obj["max_tokens"]; has {
		return
	}
	model, _ := obj["model"].(string)
	if v, ok := maxOut[model]; ok && v > 0 {
		obj["max_tokens"] = v
	}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// requestedEffort 读取客户端原始请求的思考档位（snake 优先，camel 兜底），
// 未携带或非字符串 → ""。与 normalizeReasoningEffort 的取值口径一致，
// 但**不**做降级、不补默认档：它只回答「客户端发了什么」。
//
// 为什么单独留一列：网关对 deepseek 缺档请求会补默认档（thinking.go），
// 之后 requested 与 effective 就再也分不开——日志只报 effective 时，
// 「客户端设了 max、日志却是 high」无法判断是客户端没发出去，还是被降级了
// （issue #43 / #18）。
func requestedEffort(obj map[string]any) string {
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if v, ok := obj[k].(string); ok {
			if s := strings.TrimSpace(strings.ToLower(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

// normalizeReasoningEffort 把请求档位对齐到模型支持的档位（snake/camel 双字段兼容），
// 返回本次最终生效的档位（未携带字段/未知模型/未知档位/模型未缓存 → ""）。
//   - 请求档位模型支持 → 原样透传
//   - 不支持 → 取**最近**的支持档；距离相同优先模型声明的默认档，其次偏高的一档
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
//
// 为什么不一律降级：Codex 等客户端默认发 medium，而 CN 模型常见档位是 low/high/max
// （没有 medium）——一律取「≤ 请求档的最高档」会降到 low，比**不携带该字段**时的模型
// 默认档 high 还弱，客户端完全无感（issue #175）。
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string, defaultEfforts map[string]string) string {
	if len(efforts) == 0 {
		return ""
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return ""
	}
	// off/none 是「关闭思考」的表达，不是可降级的档位。没有模型声明 off 档位
	// （effortRank 里 rank 0 的 off 只是排序占位），不截住就会被就近降级成最弱
	// 正档——客户端要关闭，网关却给它开了最弱的思考（实测 glm 变 low）。
	// deepseek 的关闭形态由 injectThinking 写成 thinking.type=disabled。
	if effortValueOff(obj) {
		delete(obj, "reasoning_effort")
		delete(obj, "reasoningEffort")
		return ""
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return ""
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return ""
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return ""
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return ""
	}
	// 取最近档：距离最小；同距优先模型默认档，其次偏高的一档。
	def := strings.ToLower(strings.TrimSpace(lookupDefaultEffort(defaultEfforts, model)))
	best, bestIdx, bestDist := "", -1, 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if !k {
			continue
		}
		dist := idx - reqIdx
		if dist < 0 {
			dist = -dist
		}
		if dist > bestDist {
			continue
		}
		if dist == bestDist {
			curDef := def != "" && strings.EqualFold(strings.TrimSpace(s), def)
			bestDef := def != "" && strings.EqualFold(best, def)
			switch {
			case curDef && !bestDef: // 默认档优先
			case curDef == bestDef && idx > bestIdx: // 同距宁高
			default:
				continue
			}
		}
		best, bestIdx, bestDist = s, idx, dist
	}
	if best == "" {
		return reqStr
	}
	if !strings.EqualFold(best, reqStr) {
		obj[key] = best
		log.Printf("reasoning_effort adjusted model=%s %s -> %s（模型支持 %s）",
			model, reqStr, best, strings.Join(supported, ","))
	}
	return best
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法。
//
// OpenAI Chat Completions 规范使用对象形态 {"url":"...","detail":"..."}，
// 部分客户端（以及 Responses -> Chat 转换器）会发送字符串形态 "data:..." 或
// "https://..."。WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象及其中 url/detail/mime_type
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值，让上游返回真实错误。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// normalizeStop 兼容 OpenAI chat 请求 stop 字段的两种合法写法。
//
// OpenAI 规范允许 stop 为字符串或字符串数组（≤4 个停止词），部分客户端（SillyTavern
// 等）只发字符串形态；但上游 Go struct 把 stop 定义成 []string，字符串形态直接解码
// 失败：400 code=11101 "cannot unmarshal string into Go struct field ***.stop of type
// []string"。网关把 11101 归 ErrBadParams（不罚号但**仍轮换**），同一 body 换任何号都
// 必然同样 400——全池试完走末端透传，客户端看到「所有账号不可用」，与账号健康无关。
//
// 只做形状转换：字符串 → 单元素数组。**不 TrimSpace**——停止词的前后空白是匹配语义
// 本身（"\n\nHuman:"、"\n\n" 靠前导换行触发），裁剪等于静默改写客户端意图。空串同样
// 转 [""] 保留（不替客户端判定什么是无效值）；数组 / null / 其他类型 / 字段缺失一律不动。
func normalizeStop(obj map[string]any) {
	s, ok := obj["stop"].(string)
	if !ok {
		return
	}
	obj["stop"] = []string{s}
}

// dropReasoningContentParts 剔除 chat content 数组里的 reasoning_text / reasoning 片段
// （吸收上游 PR #159）。
//
// 场景：Codex 只走 /v1/responses，第三方中转的 responses→chat 转换器（如 new-api 的
// openai_responses_to_openai_chat_completions）会把助手历史里的思考片段原样透传进
// /v1/chat/completions；上游 content 类型白名单不含它，命中即 HTTP 400 code=11101
// 「unsupported content type at index 0: reasoning_text」。本仓自己的 responses 入站
// 不产这种片段（客户端回传的 reasoning item 在 convertInputItems 里就丢弃了），这条
// 是给「链路上游还有别的转换器」兜底。
//
// 思考片段只是模型私有推理，下游续写不需要，剔除无副作用：
//   - 删除 type 为 reasoning_text / reasoning 的片段（TrimSpace 后比较）
//   - 删除后 content 为空且该消息无 tool_calls → 整条消息一并删除；有 tool_calls 则保留
//     （留空数组，避免破坏工具调用配对）
func dropReasoningContentParts(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(msgs))
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			out = append(out, rawMsg) // 字符串 content（或字段缺失）不受影响
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				kept = append(kept, rawPart)
				continue
			}
			ptype, _ := part["type"].(string)
			switch strings.TrimSpace(ptype) {
			case "reasoning_text", "reasoning":
				continue
			}
			kept = append(kept, rawPart)
		}
		if len(kept) == 0 {
			if _, hasToolCalls := msg["tool_calls"]; !hasToolCalls {
				continue // 整条只剩思考片段：删掉，否则上游会为这条空 content 报错
			}
		}
		msg["content"] = kept
		out = append(out, rawMsg)
	}
	obj["messages"] = out
}

// consoleSystemFallback 兜底 system 正文：只写行为约定，不写身份（issue #187）。
const consoleSystemFallback = "Follow the user's instructions and respond in the user's language."

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45，防 console 域上游 code 11-128）：
// 首条消息非 system 时在 messages 最前补一条 fallback system。仅对 global 请求调用
// （CN 现状不动；即使首条就是 system 也不重复注入）。
//
// 兜底文本**不声明身份**（issue #187）：上游的判据只是「首条必须是 system」，正文不参与
// 校验；写成「You are a helpful assistant.」等于网关自己往对话里插一句身份声明，与客户端
// harness 自带的人设并列时，模型会把它当成外部注入而拒绝执行。这里只声明行为约定，
// 不碰身份。
//
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": consoleSystemFallback}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeToolPatterns 归一化 tools 子树里 pattern 的非标准转义 `\_`（→ `_`）。
//
// 上游对 tools[].function.parameters 做严格 JSON Schema/正则文法校验，pattern 含
// `\_`（转义的字面量下划线）会整体拒收：400 code=11129 invalid_function_call_
// parameters（displayMsg「工具定义不合规」）。`\_` 不是任何正则文法的合法转义，
// 但所有主流引擎（RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们
// 全部更严（对照 V8 严格文法 u 标志，唯一同样拒绝的实现）。实案：ZCode 的 exa 插件
// agent_run 工具 runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400
// → 网关侧归 ErrClient 只换号不罚但喂连败计数 → 轮转烧满 5 连败触发连败降权、
// 客户端 503（2026-09-29/30 两次实案）。schema 级拒绝换账号无用，只能在发送前修。
//
// 归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照探针：归一后
// 200），工具方功能不变。只动 tools 子树（pattern 值 + patternProperties 键）；
// 消息正文里的 `\_`（如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实
// 触发，不扩面——有实案再议。独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI 形态 tools[].function.parameters；裸 tools[].parameters 兼容。
		if fn, ok := tool["function"].(map[string]any); ok {
			normalizeSchemaTree(fn["parameters"])
		}
		normalizeSchemaTree(tool["parameters"])
	}
}

// normalizeSchemaTree 递归归一化工具参数 schema 树：
//   - pattern 值与 patternProperties 键里的 `\_` → `_`（patternProperties 的键也是正则；
//     map 键不可原地改，命中时重建该层）；
//   - 只有 enum、没有 type 的属性补上可无损推断的类型（见 inferEnumType）。
func normalizeSchemaTree(node any) {
	switch n := node.(type) {
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}
		// enum 缺 type：Kimi 系对参数 schema 严格校验，直接 400 11133「Invalid request
		// parameters」——同一个模型对带这类 schema 的客户端等于全废。类型能无损推断就补上，
		// 推断不出来（混合标量/含 null）保持原样，让上游按原语义报错，不猜。
		if _, hasType := n["type"]; !hasType {
			if e, ok := n["enum"].([]any); ok && len(e) > 0 {
				if t := inferEnumType(e); t != "" {
					n["type"] = t
				}
			}
		}
		for _, v := range n {
			normalizeSchemaTree(v)
		}
	case []any:
		for _, v := range n {
			normalizeSchemaTree(v)
		}
	}
}

// inferEnumType 从 enum 取值推断 JSON Schema 类型；推不出唯一类型时返回空（不改写）。
// 数字经 encoding/json 解出来一律是 float64，故按「全为整数值」区分 integer / number。
func inferEnumType(values []any) string {
	var strs, nums, ints, bools int
	for _, v := range values {
		switch x := v.(type) {
		case string:
			strs++
		case bool:
			bools++
		case float64:
			nums++
			if x == math.Trunc(x) {
				ints++
			}
		default:
			return "" // null / 对象 / 数组：类型不唯一，不猜
		}
	}
	n := len(values)
	switch {
	case strs == n:
		return "string"
	case bools == n:
		return "boolean"
	case nums == n && ints == n:
		return "integer"
	case nums == n:
		return "number"
	}
	return ""
}
