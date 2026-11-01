// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/reqlog"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// maxUserAgentLen 归档与面板展示保留的 UA 字节上限。UA 是客户端完全可控的
// 自由文本（浏览器动辄 150+ 字符，恶意客户端可以塞几 KB），落盘前必须截断，
// 否则一条请求就能把归档行撑大。截断只影响展示，不影响请求处理。
const maxUserAgentLen = 200

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start time.Time
	model string
	mode  string // "stream" | "sync"
	uid   string // 完整 uid，展示时只取前 8 位
	nick  string // 账号昵称（随选号同步），流水行经 logfmt.Label 拼成 "昵称(uid8)"
	ttfb  time.Duration
	toks  int // <0 表示 usage 缺失 → 显示 "-"
	// think：本次 completion_tokens 里有多少是思考 token（上游把思考算进 completion）。
	// <0 = 上游没给该字段（非推理模型/旧形态），此时行里不显示 think=。
	think     int
	status    int
	requestID string
	outcome   string
	attempts  int
	credit    float64
	hasCredit bool
	// promptTokens/completionTokens/totalTokens 供请求指标归档（每次 attempt 覆盖）。
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	cacheHit         int
	cacheMiss        int
	hasCache         bool
	budget           *dailyBudget

	// 调用来源（客户端 IP / User-Agent）。空 = 未采集（logging.request_client_info
	// 关闭，或非 chat 路径），展示层一律以 "-" 兜底。
	clientIP  string
	userAgent string

	// toolsDeclared / toolCalls 本轮工具面观测量（issue #44）：声明数与实际调用数。
	// 前者 >0 且后者 ==0 即「零工具调用收尾」，归档里可据此算发生率。
	toolsDeclared int
	toolCalls     int

	// 上游调用分段耗时（见 upstream.ChatTiming）：prep 本地准备、do 上游往返。
	// 零值 = 未测量（非流式/未走到上游）。
	prep, do time.Duration

	// effort 本次实际生效的思考档位（降级后出站值）。
	// effortReq 客户端原始请求档位（injectThinking 补默认档之前抓的）。
	// 两者分开的原因见 issue #43：只报生效值时，「客户端设了 max、日志显示 high」
	// 无法判断是客户端根本没发出去，还是被网关按模型档位表降级了。
	effort    string
	effortReq string

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
// model 由调用方传入已解析的 peek.Model（空串按 "-" 展示），避免再解析一次 body。
func newChatStat(now time.Time, model string, stream bool, budget *dailyBudget) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	if model == "" {
		model = "-"
	}
	return &chatStat{start: now, model: model, mode: mode, toks: -1, think: -1, budget: budget}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	total := time.Since(s.start)
	logChatRowFull(s.ttfb, total, s.model, s.mode, s.uid, s.nick, s.status, s.toks,
		s.think, s.cacheHit, s.cacheMiss, s.hasCache,
		s.requestID, s.outcome, s.attempts, s.credit, s.hasCredit,
		s.clientIP, s.userAgent, s.prep, s.do, s.effort, s.effortReq)
	s.budget.add(s.credit, s.hasCredit)
	s.budget.observeCache(int64(s.cacheHit), int64(s.cacheMiss), s.hasCache)
	noteProm(s, total)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br                  *bufio.Reader
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// credit 上游末帧 usage.credit（本次真实扣费积分），供成本台账（NoteModelCost）。
	hasCredit    bool
	credit       float64
	cacheHit     int
	cacheMiss    int
	hasCache     bool
	hasCacheHit  bool
	hasCacheMiss bool
	// thinkTokens 末帧 usage 里的**思考 token 数**（上游把思考算进 completion_tokens：
	// 实测 completion=100 里 91 是思考）。用于解释「速率看着慢」——Codex 这类高推理档
	// 流量的 completion 里思考占七到八成，正文只占小头。
	thinkTokens int
	hasThink    bool
	errorFrame  bool
	// issue #44 观测：本流是否出现过非空 delta.tool_calls、最后一个非空 finish_reason。
	// 两者组合用于识别「声明了 tools 却拿到 stop + 零 tool_calls」的静默回合。
	sawToolCalls bool
	finishReason string
	pend         []byte // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Thinking 返回末帧 usage 里的思考 token 数（上游字段 completion_thinking_tokens，
// 兼容 completion_tokens_details.reasoning_tokens）；两个都缺时 ok=false。
func (s *chatStatsReader) Thinking() (int, bool) { return s.thinkTokens, s.hasThink }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// SawToolCalls 报告本流是否出现过非空 delta.tool_calls（issue #44 观测）。
func (s *chatStatsReader) SawToolCalls() bool { return s.sawToolCalls }

// FinishReason 本流最后一个非空 finish_reason；上游没给过则为空串。
func (s *chatStatsReader) FinishReason() string { return s.finishReason }

// Credit 返回末帧 usage.credit（本次真实扣费积分）与是否缺失。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens 返回末帧 usage.total_tokens 与是否缺失。
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	hit, miss, hasCache := s.Cache()
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
		HasCacheTokens:      hasCache,
		CacheHitTokens:      int64(hit),
		CacheMissTokens:     int64(miss),
		HasCredit:           s.hasCredit,
		Credit:              s.credit,
	}
}

// Cache 返回末帧 usage 的 prompt 缓存命中/未命中 token 与是否缺失。
// miss 缺失时按 prompt-hit 推导（与上游 issue #92 同步口径）；hit 与 miss 均不可得
// 时 ok=false（不参与命中率统计）。
func (s *chatStatsReader) Cache() (hit, miss int, ok bool) {
	if !s.hasCacheHit && !s.hasCacheMiss {
		return 0, 0, false
	}
	hit, miss = s.cacheHit, s.cacheMiss
	if !s.hasCacheMiss {
		if s.hasPromptTokens && s.promptTokens >= s.cacheHit {
			miss = s.promptTokens - s.cacheHit
		} else {
			miss = 0 // 推导不可信（prompt 缺失/矛盾）：留 0，不计假数据
		}
	}
	return hit, miss, true
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Error json.RawMessage `json:"error"`
		// issue #44：只取 delta.tool_calls 与 finish_reason 两个字段。encoding/json
		// 会跳过未声明的键（含 content），不为正文分配内存。
		Choices []struct {
			Delta struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			// 思考 token：上游两个字段都出现过（顶层 completion_thinking_tokens 与
			// 嵌套 completion_tokens_details.reasoning_tokens），取到哪个用哪个。
			CompletionThinkingTokens *int `json:"completion_thinking_tokens"`
			CompletionTokensDetails  *struct {
				ReasoningTokens *int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			TotalTokens           json.RawMessage `json:"total_tokens"`
			Credit                json.RawMessage `json:"credit"`
			PromptCacheHitTokens  *int            `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens *int            `json:"prompt_cache_miss_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	if len(chunk.Error) > 0 {
		s.errorFrame = true
	}
	for _, c := range chunk.Choices {
		// 上游每帧都带 "tool_calls":[]（空数组），所以判据是长度而不是字段存在。
		if len(c.Delta.ToolCalls) > 0 {
			s.sawToolCalls = true
		}
		if c.FinishReason != "" {
			s.finishReason = c.FinishReason
		}
	}
	if chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	// 思考 token（两个字段都在就取顶层，上游顶层口径更直接）。
	if v := chunk.Usage.CompletionThinkingTokens; v != nil {
		s.hasThink = true
		s.thinkTokens = *v
	} else if d := chunk.Usage.CompletionTokensDetails; d != nil && d.ReasoningTokens != nil {
		s.hasThink = true
		s.thinkTokens = *d.ReasoningTokens
	}
	// total_tokens / credit 走「先判合法性再采信」：上游偶发小数字段或负值
	// （旧版直接断言 *int / *float64 会静默截断/采信脏值）。非法值置为「未知」
	// 并覆盖先前帧——迟到的坏帧不得把早帧的费用当成本响应的账（上游 PR #104）。
	if len(chunk.Usage.TotalTokens) > 0 {
		var total float64
		if json.Unmarshal(chunk.Usage.TotalTokens, &total) == nil {
			parsedTotal, valid := parseUsageTotalTokens(total)
			if !valid {
				s.hasTotalTokens = false
				s.totalTokens = 0
			} else {
				s.hasTotalTokens = true
				s.totalTokens = parsedTotal
			}
		} else {
			s.hasTotalTokens = false
			s.totalTokens = 0
		}
	}
	if len(chunk.Usage.Credit) > 0 {
		var credit float64
		if json.Unmarshal(chunk.Usage.Credit, &credit) == nil && validUsageCredit(credit, true) {
			s.hasCredit = true
			s.credit = credit
		} else {
			// 非法 credit 覆盖先前值：不得把早帧的费用归档成本响应的账。
			s.hasCredit = false
			s.credit = 0
		}
	}
	if chunk.Usage.PromptCacheHitTokens != nil {
		s.hasCache = true
		s.hasCacheHit = true
		s.cacheHit = *chunk.Usage.PromptCacheHitTokens
	}
	if chunk.Usage.PromptCacheMissTokens != nil {
		s.hasCache = true
		s.hasCacheMiss = true
		s.cacheMiss = *chunk.Usage.PromptCacheMissTokens
	}
}

// SawErrorFrame 报告流中是否透传过 SSE error 帧。
func (s *chatStatsReader) SawErrorFrame() bool { return s.errorFrame }

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	if n, ok := read("prompt_cache_hit_tokens"); ok {
		delta.HasCacheTokens, delta.CacheHitTokens = true, n
	}
	missSet := false
	if n, ok := read("prompt_cache_miss_tokens"); ok {
		delta.HasCacheTokens, delta.CacheMissTokens = true, n
		missSet = true
	}
	// miss 缺失时按 prompt-hit 推导（issue #92 同步口径）；推导不可信则留 0。
	if !missSet && delta.HasCacheTokens && delta.CacheHitTokens > 0 {
		if p, okP := read("prompt_tokens"); okP && p >= delta.CacheHitTokens {
			delta.CacheMissTokens = p - delta.CacheHitTokens
		}
	}
	// 扣费积分（用量视图与成本台账同源）。这里不要求 total_tokens>0——
	// 历史口径要的是"这一笔花了多少"，拿不到 token 数不该把积分一起丢掉。
	if c, ok := u["credit"].(float64); ok {
		delta.HasCredit, delta.Credit = true, c
	}
	return delta
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
// toolCallCount 非流式聚合响应里的工具调用个数（issue #44 观测用）。
// 只计数，不解析名字与参数。
func toolCallCount(resp map[string]any) int {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return 0
	}
	ch, _ := choices[0].(map[string]any)
	msg, _ := ch["message"].(map[string]any)
	// upstream.Aggregate 组包用 []map[string]any，直解上游 JSON 是 []any —— 两种都得认，
	// 否则非流式这一半永远是 0（issue #44 的比率就会被算高一倍）。
	if tcs, ok := msg["tool_calls"].([]map[string]any); ok {
		return len(tcs)
	}
	tcs, _ := msg["tool_calls"].([]any)
	return len(tcs)
}

func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 实现委托 logfmt.UID8，避免 "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

type requestTraceKey struct{}

// requestTrace 在一次 chat 请求内共享标识与最终统计，ServeHTTP 出口统一记账。
type requestTrace struct {
	id    string
	start time.Time
	stat  *chatStat
	// path 本次请求的真实入站路径（isChatEntry 四条之一），归档事件按它记录——
	// 来源采集与归档已覆盖 /v1/responses、/v1/messages，不能再写死 chat 路径。
	path string
	// 调用来源，进入 handler 时一次性采集（见 ServeHTTP / captureClientInfo）。
	clientIP  string
	userAgent string
	// context 本轮会话上下文（见 archiveContext），由 logging.request_archive_context
	// 决定是否填充；关掉时保持 nil，事件里不出现该字段。
	context json.RawMessage
}

// captureClientInfo 采集调用来源（客户端 IP + 截断后的 UA）。开关关闭时保持空串：
// 来源信息比 token 计数敏感，是否落盘由 logging.request_client_info 决定。
func (t *requestTrace) captureClientInfo(r *http.Request) {
	if t == nil || r == nil {
		return
	}
	t.clientIP = clientIPForLog(r)
	t.userAgent = logfmt.Truncate(r.UserAgent(), maxUserAgentLen)
}

// contextMaxBytes 单个事件的上下文上限。内存只留 100 条事件，16KB × 100 = 1.6MB
// 封顶；归档里也是同一份（超长上下文按「丢最旧的轮次」收敛，不截成半个 JSON）。
const contextMaxBytes = 16 << 10

// archiveContext 从入站请求体里抽出会话上下文（system + messages，或出图/embedding
// 的 prompt），供面板「请求事件日志」展开查看。开关关闭时调用方不调用本函数。
//
// 只取这三段：tools / 采样参数 / 传输字段与「上下文」无关，体积还可能吃掉整个预算。
// 超限时丢最旧的消息，仍超就把最后一条的 content 截断——保持输出始终是合法 JSON，
// 面板按结构渲染而不是按截断的文本渲染。
func archiveContext(body []byte) json.RawMessage {
	var in struct {
		System   any   `json:"system"`
		Messages []any `json:"messages"`
		Prompt   any   `json:"prompt"`
	}
	if json.Unmarshal(body, &in) != nil {
		return nil
	}
	out := map[string]any{}
	if in.System != nil {
		out["system"] = in.System
	}
	if in.Prompt != nil {
		out["prompt"] = in.Prompt
	}
	for len(in.Messages) > 0 {
		out["messages"] = in.Messages
		b, err := json.Marshal(out)
		if err != nil {
			return nil
		}
		if len(b) <= contextMaxBytes {
			return b
		}
		if len(in.Messages) > 1 {
			in.Messages = in.Messages[1:] // 丢最旧一轮，保留最近对话
			continue
		}
		// 单条就超预算（贴了一大段代码）：把 content 截成串。非字符串（多模态块数组）
		// 不再拆——整条返回，超出的由上游 context 上限决定，不在这里改结构。
		if msg, ok := in.Messages[0].(map[string]any); ok {
			if s, ok := msg["content"].(string); ok {
				msg["content"] = logfmt.Truncate(s, contextMaxBytes/2) + "…（超长已截断）"
				if b, err := json.Marshal(out); err == nil {
					return b
				}
			}
		}
		break
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	// 没有 messages、只有超长 system / prompt（大段 system 提示词）时上面那条收敛路径
	// 不触发，这里兜一次，否则单个事件能撑到几 MB 进归档。
	if len(b) > contextMaxBytes {
		for _, k := range []string{"system", "prompt"} {
			if s, ok := out[k].(string); ok {
				out[k] = logfmt.Truncate(s, contextMaxBytes/2) + "…（超长已截断）"
			}
		}
		if b, err = json.Marshal(out); err != nil {
			return nil
		}
	}
	return b
}

// clientIPForLog 提取用于日志展示的客户端 IP。
//
// 与 upstream.ExtractClientIP 的差别：后者只认代理头（X-Forwarded-For 首段 →
// X-Real-IP），因为它的用途是把客户端 IP **透传给上游**，回落到网关自身地址会
// 污染上游风控判据；日志场景相反——直连（无反代）时 RemoteAddr 就是唯一线索，
// 必须回落，否则面板里所有来源都显示 "-"。代理头优先保证反代后拿到真实客户端。
func clientIPForLog(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// dashIfEmpty 空串统一显示 "-"（来源字段未采集时不留空白列）。
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func requestTraceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	tr, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return tr
}

func (t *requestTrace) event(status int) reqlog.Event {
	e := reqlog.Event{
		Time:      t.start,
		RequestID: t.id,
		Path:      t.path,
		Status:    status,
	}
	duration := time.Since(t.start)
	e.DurationMs = duration.Milliseconds()
	if e.DurationMs < 1 {
		e.DurationMs = 1
	}
	if t.stat != nil {
		s := t.stat
		e.Account = logfmt.Label(s.uid, s.nick)
		e.Model = s.model
		e.Outcome = s.outcome
		e.TTFBMs = s.ttfb.Milliseconds()
		e.Attempts = s.attempts
		e.PromptTokens = s.promptTokens
		e.CompletionTokens = s.completionTokens
		e.TotalTokens = s.totalTokens
		e.Credit = s.credit
		e.HasCredit = s.hasCredit
		// 缓存命中/未命中（issue #92 同步）：hasCache 未置位时为零值，omitempty 省略。
		e.CacheHitTokens = int64(s.cacheHit)
		e.CacheMissTokens = int64(s.cacheMiss)
		e.Mode = s.mode
		e.ReasoningEffort = s.effort
		e.ReasoningEffortRequested = s.effortReq
		if s.think >= 0 {
			e.ReasoningTokens = int64(s.think)
		}
		e.ToolsDeclared = s.toolsDeclared
		e.ToolCalls = s.toolCalls
	}
	e.ClientIP = t.clientIP
	e.UserAgent = t.userAgent
	e.Context = t.context
	e.ExecutorType = deriveExecutorType(t.userAgent)
	if e.Outcome == "" {
		if status >= 200 && status < 300 {
			e.Outcome = reqlog.OutcomeSuccess
		} else {
			e.Outcome = reqlog.OutcomeHTTPError
		}
	}
	e.OK = status >= 200 && status < 300 && e.Outcome == reqlog.OutcomeSuccess
	return e
}

// responseObserver 捕获 handler 实际写出的 HTTP 状态，同时保留 Flusher/Unwrap，
// 避免破坏 SSE 逐帧刷新。
type responseObserver struct {
	http.ResponseWriter
	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名："global:" (7) + "deepseek-v4.1-flash" (19) = 26。
	// 旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成 "cn:deepseek"，让人误以为是另一个模型。
	chatModelWidth = 26
	// chatAcctWidth 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列。
	chatAcctWidth  = 22
	chatTTFBWidth  = 8
	chatTokWidth   = 6
	chatRateWidth  = 11 // 形如 "183.6tok/s"
	chatCacheWidth = 6  // 形如 "97%" / "-"
)

// logChatRow 打印一行请求级表格日志（输出 chatLogOut，无 log 时间戳前缀）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 chatModelWidth 截断（模型名是 ASCII，字节截即列宽）；
//   - uid/nick：完整 uid 与账号昵称，经 logfmt.Label 拼成 "昵称(uid8)" 展示——只有
//     uid8 时人眼无法判断是哪个号，要辨认必须再查 auths/，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"。
//
// logChatRow 旧形态（带缓存命中率、无请求 ID）；新代码请用 logChatRowFull。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int, cacheHit, cacheMiss int, hasCache bool) {
	logChatRowFull(ttfb, total, model, mode, uid, nick, status, toks, -1, cacheHit, cacheMiss, hasCache, "", "", 0, 0, false, "", "", 0, 0, "", "")
}

// logChatRowEx 带请求 ID、结果、重试、积分与调用来源字段的扩展形态；缓存列不传
// （显示 "-"）；来源两参均为空时不追加来源段。
func logChatRowEx(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int,
	requestID, outcome string, attempts int, credit float64, hasCredit bool,
	clientIP, userAgent string) {
	logChatRowFull(ttfb, total, model, mode, uid, nick, status, toks, -1, 0, 0, false,
		requestID, outcome, attempts, credit, hasCredit, clientIP, userAgent, 0, 0, "", "")
}

// logChatRowFull 是本文件唯一的行输出点：宽表字段 + 缓存列 + 可选请求 ID/结果/重试/积分尾段 + 调用来源段。
//
// effort 为生效档位、effortReq 为客户端原始档位（见 chatStat 字段注释）：
// 两者相同只报 effort=；不同报 effort=max->high（客户端要什么、实际发出去什么
// 一眼可见）；客户端关思考（off/none/disabled）时生效值被清空，报客户端原值
// effort=off；连档位都没带时报 effort=-。
func logChatRowFull(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int,
	think int, cacheHit, cacheMiss int, hasCache bool,
	requestID, outcome string, attempts int, credit float64, hasCredit bool,
	clientIP, userAgent string, prep, do time.Duration, effort, effortReq string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号标签只补不截：超宽时宁可让该行变宽，也不丢昵称信息（昵称是排查的主线索）。
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		// 思考 token 单列：上游把思考算进 completion_tokens（实测 521 里 431 是思考），
		// 不分开看就分不清「模型慢」和「模型在想」——Codex 这类高推理档流量尤其明显。
		if think >= 0 {
			tokField += fmt.Sprintf(" think=%d", think)
		}
		// 速率与用量账本走同一个函数（扣掉 TTFB）。此前这里自己除 total，漏扣首
		// token 等待，于是控制台流水行的 tok/s 与面板数字对不上（吸收上游 8b18ab8）
		// ——本函数本来就收到了 ttfb，只是没拿它算速率。
		// 无 TTFB 观测（非流式）时不报速率：分母含预填，读数不可比，留 "-"。
		if tps, ok := tokensPerSecond(int64(toks), total, ttfb); ok {
			tokpsField = fmt.Sprintf("%.1ftok/s", tps)
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	// 缓存命中率=hit/(hit+miss)。上游没给缓存字段时显示 "-"（不伪造 0%）。
	cacheField := "-"
	if hasCache && cacheHit+cacheMiss > 0 {
		cacheField = fmt.Sprintf("%d%%", cacheHit*100/(cacheHit+cacheMiss))
	}
	// 分段耗时尾段（见 upstream.ChatTiming）：把 TTFB 拆成「本地准备 prep + 上游往返 do」。
	// 只在本跳真测量过时输出——非流式/未启用留空，旧行格式不变。
	split := ""
	if do > 0 || prep > 0 {
		split = fmt.Sprintf(" prep=%dms do=%dms", prep.Milliseconds(), do.Milliseconds())
	}
	// 思考档位（见 upstream.normalizeReasoningEffort）：每一行都给出，便于对照
	// 「客户端要什么 / 网关实际发什么 / 根本没档位」三类情况：
	//   - 降级：effort=max->high（issue #43：只报生效值会被读成客户端没发 max）
	//   - 客户端关思考（off/none/disabled）：生效档位会被清空，显示客户端原值 effort=off
	//   - 客户端没带、网关补齐了默认档：effort=- ->max（"-" 是客户端侧；别读成降级）
	//   - 完全没档位：effort=-（网关关闭策略或模型不支持档位时就是这一种）
	switch {
	case effort != "" && effortReq != "" && !strings.EqualFold(effortReq, effort):
		split += " effort=" + effortReq + "->" + effort
	case effort != "" && effortReq != "":
		split += " effort=" + effort
	case effort != "":
		split += " effort=- ->" + effort
	case effortReq != "":
		split += " effort=" + effortReq
	default:
		split += " effort=-"
	}
	extra := ""
	if requestID != "" {
		if outcome == "" {
			outcome = reqlog.OutcomeHTTPError
			if status >= 200 && status < 300 {
				outcome = reqlog.OutcomeSuccess
			}
		}
		creditField := "-"
		if hasCredit {
			creditField = fmt.Sprintf("%.4f", credit)
		}
		extra = fmt.Sprintf(" rid=%s | out=%s | try=%d | credit=%s |", requestID, outcome, attempts, creditField)
	}
	// 调用来源：IP 用可解析的裸值（便于 grep），UA 用 ShortUA 压缩后的客户端标签
	// 并加引号（标签内可能含空格，如 `OpenAI/Python 1.30.0` 只会取到 OpenAI/Python）。
	// 两者都未采集时不追加，旧行格式保持不变。
	src := ""
	if clientIP != "" || userAgent != "" {
		ua := "-"
		if s := logfmt.ShortUA(userAgent); s != "" {
			ua = `"` + s + `"`
		}
		src = fmt.Sprintf(" src=%s ua=%s |", dashIfEmpty(clientIP), ua)
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | cache=%s | total=%.1fs |%s%s%s\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		logfmt.Pad(cacheField, chatCacheWidth),
		total.Seconds(),
		split,
		extra,
		src,
	)
}

// cacheMissSignal 低命中率告警信号（issue #92 同步 P1 轻量化）：同一模型此前观测到
// 过命中、本次大前缀（≥1000 tok）整段未命中（命中率 <10%）时打一条 WARN，每模型
// 10 分钟冷却。只报「本可命中却重算」的可行动信号——新模型/新会话的首请求天然
// 全 miss，不在此列。
type cacheMissSignal struct {
	mu      sync.Mutex
	everHit map[string]bool
	last    map[string]time.Time
}

var cacheMissWarn = &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}

// cacheMissWarnEvery 冷却间隔；变量供测试缩短。
var cacheMissWarnEvery = 10 * time.Minute

// noteCacheTokens 记录一次观测：有命中 → 标记该模型可命中；整段未命中且此前
// 命中过 → 触发 WARN（冷却内不重复）。
func (c *cacheMissSignal) noteCacheTokens(model string, prompt, hit, miss int64) {
	if model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if hit > 0 {
		c.everHit[model] = true
		return
	}
	// hit==0 才可能是整段重算。小前缀（<1000 tok）的 miss 无告警价值；上游未回
	// miss 字段时按 prompt 全量视为未命中。
	if prompt < 1000 {
		return
	}
	if miss <= 0 {
		miss = prompt
	}
	// 判据是「重算占比」而非「hit 为 0」：上游会只报极小的重算量（实测 miss=3 /
	// prompt=52703，命中率 99.99%），此时 hit=0 只说明本帧没有命中的 token，不代表
	// 大前缀被整段重算（issue #170 假阳性，会淹没真告警）。
	if miss*2 < prompt {
		return
	}
	if !c.everHit[model] {
		return
	}
	now := time.Now()
	if t, ok := c.last[model]; ok && now.Sub(t) < cacheMissWarnEvery {
		return
	}
	c.last[model] = now
	log.Printf("WARN: [server] cache miss: model=%s 本次大前缀未命中（prompt=%d miss=%d，此前观测到过命中）；上游前缀缓存重算，费用会显著升高", model, prompt, miss)
}

func deriveExecutorType(ua string) string {
	lower := strings.ToLower(ua)
	switch {
	case strings.Contains(lower, "antigravity"):
		return "AntigravityExecutor"
	case strings.Contains(lower, "codex"):
		return "CodexExecutor"
	case strings.Contains(lower, "claude"):
		return "ClaudeCodeExecutor"
	case strings.Contains(lower, "cursor"):
		return "CursorExecutor"
	case strings.Contains(lower, "continue"):
		return "ContinueExecutor"
	case strings.Contains(lower, "asyncopenai") || strings.Contains(lower, "openai") || strings.Contains(lower, "python"):
		return "OpenAIClient"
	case strings.Contains(lower, "curl") || strings.Contains(lower, "http"):
		return "HTTPClient"
	default:
		if ua != "" {
			parts := strings.Split(ua, "/")
			if len(parts) > 0 && len(parts[0]) <= 20 {
				return parts[0] + "Executor"
			}
		}
		return "WorkBuddyExecutor"
	}
}
