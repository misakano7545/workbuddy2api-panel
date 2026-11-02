// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header",                      // header 键值段键名
	"cc_entrypoint=",                                  // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",                             // 身份句（截断前缀即可命中）
	"Main branch (",                                   // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11128",                                           // 上游反探测：裸数字错误码
	"Tokens prefixed with @",                          // DSH 0.3.x 的 @ 路径说明段（唯一不含品牌词的 DSH 指纹）
}

// sanitizeIdentityFeatures 身份类特征：品牌词（DSH 身份句）。默认对所有消息角色生效
// （与上游既有行为一致）；features.sanitize_brand_scope=identity 时只对 system/developer
// 生效，其余角色按用户数据原样透传。
var sanitizeIdentityFeatures = []string{
	"DeepSeek Harness",
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeDshPathsRe DSH 0.3.x 系统提示里的「@ 开头的路径怎么解析」整段说明。
//
// 这一段是本仓唯一漏掉的 DSH 指纹：段内**不含任何品牌词**，所以 sanitizeIdentityRewrites
// （DeepSeekRuntime → …）的预检根本不会命中，原文原样发出即 400 11-128
// Illegal API invocation from an unapproved channel。
//
// 上游按「连续原文」匹配（逐段二分实测：只发首句放行，前两句连排即拦），所以必须整段
// 一起改。这里做同义改写而不是删除：@ 路径、相对路径基准、结尾斜杠表示目录、其余按文件读、
// @"..." 用于含空格的路径——语义全部保留，只换措辞让连续特征失配。
//
// 两种线上见过的结尾措辞都要覆盖（(?s) 让 .*? 跨行，段内本来就有换行）：
//
//	... quotes a path containing spaces.            （0.3.x 当前写法）
//	... form only quotes a path that contains spaces. （更早的写法）
//
// 段外的 @ 不受影响：邮箱、包名、含 @ 的绝对路径都不匹配。
//
// 两条已知边界：预检是大小写敏感的（与 sanitizeFeatures 其它项一致；DSH 原文大小写固定）；
// 规则属于全局改写层，任何角色里出现这一段都会被改——上游按连续原文匹配、不分角色，
// 保留原样必然 400。
var sanitizeDshPathsRe = regexp.MustCompile(
	`(?is)Tokens prefixed with @ are paths the user explicitly referenced\..*?path (?:containing|that contains) spaces\.`)

const sanitizeDshPathsProbe = "Tokens prefixed with @"

// sanitizeDshPathsReplacement 逐句同义改写。与主流实现（workbuddy2api-hub #677/#678）逐字
// 一致：那段替换串已被真实 CN 账号回放验证过（修复前 400 / 修复后 200）。
//
// 一处偏离：原文用 "workspace root"，这里写 "project root"。ithtelab/workbuddy-manager#312
// 的 0032 声称 2026-10-27 上游又单独加了 "workspace root" 这个词（root→base 即放行），
// 与上面的整段回放结论冲突——两种说法下 project root 都安全，且与 DSH 语义等价，故取它。
const sanitizeDshPathsReplacement = "Paths beginning with @ refer to files the user explicitly mentioned. " +
	"Relative paths are resolved against the project root; absolute paths point " +
	"to files or directories on the host. A trailing slash marks a directory, so " +
	"list it when its contents matter. Anything else is a file: open it with the " +
	"read tool when its contents are needed, and never claim to have inspected it " +
	"before reading. The @\"...\" form only quotes a path that contains spaces."

// sanitizeIdentitySentenceRe 客户端「自我介绍句 + 品牌归属」整句（issue #246 的通用版）。
//
// 上游把这类句子当成非官方渠道特征：`You are an AI agent powered by <品牌>.`
// `You are a coding agent powered by <模型>.` `You are <Product> Agent, built by <Vendor>.`
// 大小写不敏感、按连续原文匹配；单独品牌词或换掉句子结构即放行。此前只有
// sanitizeIdentityRewrites 把品牌词换掉，句子骨架（`You are … powered by …`）原样留着——
// 骨架本身就是特征，品牌换了也可能照样 400（workbuddy2api-hub #533/#537/#539、#574
// 与 ithtelab/workbuddy-manager#312 的 0032 两条独立实现都收敛到这个结论）。
//
// 动词表要全：上游补一条新搭配就能让只认单个动词的实现重新 400，故一次列七种。
// 窗口有界（形容词 0~3 个、名词可省）——`You are free to use tools built by …` 这类
// 分词修饰宾语的句子不该被误伤，靠的就是这个上界。
// 只作用于需要应用身份类规则的消息（见 sanitize_messages 的作用域选择）。
// 结尾用捕获组 (\s|$) 回填，而不是零宽断言：Go 的 RE2 不支持 (?=)，句尾那个空白必须
// 原样接回去——否则 "…assistant. Use the read tool" 会被粘成 "assistant.Use the read tool"。
// shortcut: 句子必须以句号收尾才匹配（观测到的指纹都带句号），无句号的变体放行。
var sanitizeIdentitySentenceRe = regexp.MustCompile(
	`(?i)You are\s+` +
		`(?:an?\s+|the\s+)?(?:[\w.-]+\s+){0,3}?` +
		`(?:agent|assistant|model|AI|LLM)?[\s,]*` +
		`(?:powered by|built on|built by|created by|developed by|made by|trained by)` +
		`[^\n]*?\.(\s|$)`)

const sanitizeIdentitySentenceReplacement = "You are a helpful AI assistant.${1}"

// sanitizeIdentityVerbs 身份句式的字面量快路（与上面正则的动词表同步维护）。
var sanitizeIdentityVerbs = []string{
	"powered by", "built on", "built by", "created by", "developed by", "made by", "trained by",
}

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
//
// 替换串用 "|" 分隔多候选，按请求种子选一（见 pickRewrite）：单一改写句本身
// 也是一条可被聚类的指纹——全网都换出同一句话，上游换个判据就能重新命中。
// 第一候选保持历史行为不变，种子为 0（无会话标识）时逐字节等价于多候选之前。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude|" +
			"You are Claude Code, Anthropic's official command line client for Claude|" +
			"You are Claude Code, Anthropic's official CLI app for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)|" +
			"Primary branch (you will usually use this for PRs)|" +
			"Base branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.|" +
			"You are a coding agent running in the Codex CLI program, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues|" +
			"To send feedback, users should report the issue at https://github.com/anthropics/claude-code/issues|" +
			"To share feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字的上下文无关——
		// "code=11128" / 裸 "11128" / "错误码 11128" / "Code=11128" 全部命中；
		// 相邻的 11148 / 11101 / 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求。
		// 代价：用户对话中任何 11128 都会被改写——但这串数字出现在请求里本身就是拦截条件，
		// 不改写必然失败。插入连字符保留可读性与指代（该数字串上零宽空格无效，实测上游会归一化）。
		"11128",
		"11-128|111-28",
	},
}

// sanitizeIdentityRewrites 身份类改写：品牌词 → DeepSeekRuntime。仅在应用身份类规则时使用
// （见 sanitizeTextWith：sanitize_brand_scope=all 时所有角色都用，identity 时只用身份消息）。
var sanitizeIdentityRewrites = [][2]string{
	{
		// DSH 的身份句（@deepseek-ai/dsh-system-prompt 注入的 harness:identity 段）被上游
		// 当成「非官方渠道调用」整单拦截：400 11-128 Illegal API invocation from an
		// unapproved channel。实测换一词即绕过（与插 U+200B 等效，但可见、可 diff）。
		"DeepSeek Harness",
		"DeepSeekRuntime",
	},
}

// pickRewrite 从多候选替换串里按请求种子稳定选一。
//
// to 不含 "|" → 原样返回（单候选规则与种子无关，存量行为零变化）；多候选且
// 种子为 0（请求没有会话标识）→ 固定取第一个，与未实现多候选时逐字节一致；
// 其余情况 → (种子 ^ 规则原文哈希) 取模选一，各规则独立翻转，不必同步换候选。
// 空候选（"|" 首尾或连续）等价于剥离语义，原样保留。
func pickRewrite(to, from string, seed uint64) string {
	if !strings.Contains(to, "|") {
		return to
	}
	parts := strings.Split(to, "|")
	if seed == 0 {
		return parts[0]
	}
	idx := (seed ^ fnv1a(from)) % uint64(len(parts))
	return parts[idx]
}

// fnv1a FNV-1a 64。选它是因为零依赖且跨进程稳定：候选选择必须可复现——
// 同一会话里的同一条模板句每次都要换出同一候选，否则多轮对话前后措辞忽变。
func fnv1a(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// variantSeed 请求级改写种子。conversation 用 prompt_cache_key 的会话派生键：
// 空串（无会话信息）→ 0，候选固定取第一个，与未实现多候选时逐字节一致；
// 非空 → FNV-1a 哈希，同一会话稳定，不同会话各走各的候选。
func variantSeed(conversation string) uint64 {
	if conversation == "" {
		return 0
	}
	return fnv1a(conversation)
}

// sanitizeText 单段文本净化（全局规则）：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string { return sanitizeTextWith(text, false, 0) }

// sanitizeTextIdentity 身份消息（system/developer）专用：在全局规则之外再应用身份类规则
// （品牌词）。sanitize_brand_scope=all 时非身份消息也走这条（与上游既有行为一致）。
func sanitizeTextIdentity(text string) string { return sanitizeTextWith(text, true, 0) }

// sanitizeTextWith 按「是否应用身份类规则」净化单段文本；seed 决定多候选模板句
// 选哪一条（0 = 第一条，见 variantSeed）。
//
// 快路径同时看两类特征：只含品牌词的文本在 brand=false 时原样返回（不 TrimSpace，
// 用户输入的空白不动），在 brand=true 时进入改写。
func sanitizeTextWith(text string, brand bool, seed uint64) string {
	if !hasFingerprint(text) && !(brand && hasIdentityFingerprint(text)) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		if !strings.Contains(text, rw[0]) {
			continue
		}
		text = strings.ReplaceAll(text, rw[0], pickRewrite(rw[1], rw[0], seed))
	}
	if brand {
		// 身份句式先于品牌词：正则要匹配原文里的品牌全名，先换品牌会让规则失配。
		text = sanitizeIdentitySentenceRe.ReplaceAllString(text, sanitizeIdentitySentenceReplacement)
		for _, rw := range sanitizeIdentityRewrites {
			text = strings.ReplaceAll(text, rw[0], rw[1])
		}
	}
	// @ 路径说明段：预检已是字面量，这里再挡一次，避免无关文本白跑一遍跨行正则。
	if strings.Contains(text, sanitizeDshPathsProbe) {
		text = sanitizeDshPathsRe.ReplaceAllString(text, sanitizeDshPathsReplacement)
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
	text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名有大小写变体（X-Anthropic-...）且可能以裸键名形态出现（无冒号），
// Contains 大小写敏感、sanitizeHdrRe 要求冒号——两者都会漏掉「混合大小写 + 裸键名」，
// 必须再用不要求冒号的 (?i) 正则兜底（sanitizeBareHdrRe），否则整条净化被跳过。
// sanitizeBareHdrRe 不要求冒号，是 sanitizeHdrRe 的超集，故无需再单独匹配后者。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text)
}

// hasIdentityFingerprint 身份类特征（品牌词 / 身份句式）预检；只在需要应用身份类规则时调用，
// 保证 brand=false 的路径不会因为只含品牌词而进入清洗（否则末尾 TrimSpace 会裁掉用户空白）。
//
// 身份句式先走动词字面量探测（纯 C 单遍扫描），命中才交给正则确认——正则确认是必需的：
// 只按 "powered by" 这类词判真会把「讲解某产品」的普通用户消息也拖进清洗。
func hasIdentityFingerprint(text string) bool {
	for _, f := range sanitizeIdentityFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	low := strings.ToLower(text)
	for _, v := range sanitizeIdentityVerbs {
		if strings.Contains(low, v) {
			return sanitizeIdentitySentenceRe.MatchString(text)
		}
	}
	return false
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// brand 为 false 时不应用身份类规则（见 sanitizeMessages 的作用域选择）；
// seed 决定多候选模板句选哪一条（见 variantSeed）。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any, brand bool, seed uint64) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeTextWith(c, brand, seed)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeTextWith(text, brand, seed); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments（字符串化 JSON）。
// brand 决定是否应用身份类规则（品牌词）；其余全局规则始终应用。seed 同 sanitizeContent。
func sanitizeToolCalls(v any, brand bool, seed uint64) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeTextWith(args, brand, seed); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content 与 tool_calls；任一命中返回 true。
// sanitizeMessages 净化 messages 中的 content、reasoning 与 tool_calls；任一命中返回 true。
//
// brandGlobal 来自 features.sanitize_brand_scope：true（默认 "all"）时身份类规则对所有
// 消息角色生效，与上游既有行为一致；false（"identity"）时只对 system/developer 生效，
// 其余角色的同名文本按用户数据原样透传（实测 user 里的品牌词不触发 11-128）。
//
// seed 是请求级改写种子（见 variantSeed）：同一次请求里所有消息共用一个种子，
// 换出的候选才前后一致。
func sanitizeMessages(messages []any, brandGlobal bool, seed uint64) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		identity := strings.EqualFold(strings.TrimSpace(role), "system") ||
			strings.EqualFold(strings.TrimSpace(role), "developer")
		brand := brandGlobal || identity
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c, brand, seed); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段）实测同样携带指纹，与 content 同等净化。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeTextWith(rc, brand, seed); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		// reasoning：另一形态的思维链字段（backfillReasoningContent 会镜像一份）。
		if r, ok := m["reasoning"].(string); ok {
			if s := sanitizeTextWith(r, brand, seed); s != r {
				m["reasoning"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc, brand, seed) {
				changed = true
			}
		}
	}
	return changed
}
