// continue.go 截断自动续写：同一账号同一模型，把多段上游流拼成一条客户端可见的连续 SSE。
//
// 背景：上游对单次响应有硬上限（实测 cn:deepseek-v4.1-flash = 32000 输出 token，
// 请求显式 50000 也被夹回 32000）。Codex 这类客户端不带输出限额，写大文件/长分析
// 在 32000 处被截：旧行为是报 incomplete 让客户端自己重试（"ERROR: Reconnecting"），
// 客户端重发几乎不带前段输出（≈重新生成）。
//
// 这里在帧级把截断点接上：某段以 finish_reason=length 结束、且本段是纯文本/推理输出
// （无工具调用分片）、客户端未带显式输出限额时，用同一账号同一模型补发
// 「已输出内容作 assistant + 续写指令」的请求（线上实测：模型从中断处无缝接续），
// 其帧直接接进同一条流；终端帧被吞、usage 跨段合并、客户端只看到一次连续输出。
//
// ponytail: 只续文本/推理段。工具参数被截断（半个 JSON）不续——拼接残缺 JSON 比
// 不续更糟，且上游管线本就丢弃残缺 tool_calls；命中该形态只记日志（等真实分布
// 决定要不要做 V2 的「剩余参数改写」）。每请求最多 maxContinueSegments 段。
//
// 续写段接缝的客户端可见性：思考在正文尚未开始时照常外泄（续着同一个 reasoning
// item 喂，见 frame()），消掉「续写段纯思考期零事件」的静默——Codex 的 provider
// 配置带 stream_idle_timeout_ms，静默超时判 responseStreamDisconnected 掐线
// （线上实测续写段静默约 122s 后客户端断开、请求记 interrupted）。正文已开始后
// 的思考仍剥离，残余空帧按 keepaliveEvery 补 SSE 注释帧（纯喂活、无语义）。
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxContinueSegments 单请求最多上游段数（1 段原生 + 最多 2 次续写）。
// 封顶防失控：连续截断会级联，3 段 ≈ 3×32000 输出。要更长改这里。
const maxContinueSegments = 3

// continueNudge 续写指令：线上实测该措辞能让模型从断点无缝接续（不重头、不道歉）。
const continueNudge = "你上一条回复因达到输出长度上限被截断。请从中断处继续输出剩余内容；不要重复已输出过的部分，不要道歉或解释，直接继续。"

// keepaliveEvery 客户端可见静默的最大间隔：超时就向流里补一条 SSE 注释帧
// （": keepalive"），只喂活连接不带语义。≤0 = 每条被吞帧都补（测试确定性用）。
var keepaliveEvery = 10 * time.Second

// continueReader 读上游 SSE 并做帧级续写拼接。它在 stats/StreamHint 之下、
// 上游 body 之上：上层看到的永远是「一条完整且恰好一个终态」的流。
// 非流式（Aggregate）路径不接线——截断与重试语义由各自路径既有逻辑负责。
type continueReader struct {
	ctx      context.Context
	up       *upstream.Client
	acct     *auth.Auth
	body     []byte // 已发往上游的原始请求体（续写时在其 messages 后追加）
	clientIP string
	meta     upstream.ChatMeta

	cur io.ReadCloser
	br  *bufio.Reader
	out []byte // 已生成待输出字节

	seg      int  // 当前段序号（0 起）
	limitSet bool // 客户端显式带了输出限额：尊重之，不续写

	finish   string   // 本段 finish_reason（续写时每段重置）
	tail     []string // finish 之后的帧：续写时丢弃，降级时原样回放
	text     strings.Builder
	reason   strings.Builder
	toolSeen bool
	errSeen  bool
	id       string

	blindRetried bool // 断流盲重试已用（每请求一次；见 canContinue）

	lastClientAt time.Time // 上次客户端可见输出/keepalive 的时刻（静默节流）

	usage map[string]any // 跨段累计 usage（数字叶子求和）
	done  bool
}

func newContinueReader(ctx context.Context, up *upstream.Client, acct *auth.Auth, body []byte, clientIP string, meta upstream.ChatMeta, rc io.ReadCloser) *continueReader {
	r := &continueReader{ctx: ctx, up: up, acct: acct, body: body, clientIP: clientIP, meta: meta, cur: rc, lastClientAt: time.Now()}
	r.br = bufio.NewReaderSize(rc, 64*1024)
	r.limitSet = bodyHasOutputLimit(body)
	return r
}

// bodyHasOutputLimit 请求体是否显式带输出限额（任一别名）。带限额 = 客户端有意
// 只要这么多（如 Claude 客户端必带 max_tokens），截断是它的预期语义，不续写。
// 解析失败按「有限额」处理（不续），宁可少续不可续错。
func bodyHasOutputLimit(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return true
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if v, ok := obj[k]; ok && v != nil {
			return true
		}
	}
	return false
}

func (r *continueReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 && !r.done {
		r.step()
	}
	if len(r.out) > 0 {
		n := copy(p, r.out)
		r.out = r.out[n:]
		return n, nil
	}
	return 0, io.EOF
}

// Close 关闭当前段 body（续写切换时已关旧段；abandon 场景由 handler defer 兜底）。
func (r *continueReader) Close() error {
	if r.cur != nil {
		return r.cur.Close()
	}
	return nil
}

// step 处理当前段的一行。与 upstream.StreamHint 的行约定保持一致：
// `data: [DONE]` 段结束；`data: <payload>` 帧；其他非空行原样透传；空行吞掉
// （帧分隔由本层自产 "\n\n"）。
func (r *continueReader) step() {
	line, err := r.br.ReadString('\n')
	trimmed := strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(trimmed, "data: [DONE]"):
		r.endSegment(true)
		return
	case strings.HasPrefix(trimmed, "data: "):
		r.frame(strings.TrimPrefix(trimmed, "data: "))
	case trimmed != "":
		r.out = append(r.out, line...)
	}
	if err != nil {
		r.endSegment(false) // 含 io.EOF 与读错误：交给 endSegment 决定续/降级/结束
	}
}

// frame 处理一个 data 帧：解析状态（正文/推理/工具/usage），决定透传、吞掉或入 tail。
func (r *continueReader) frame(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		// 非法 JSON：原样透传（不解析状态，防抖）
		r.emit(payload)
		return
	}
	if _, hasErr := obj["error"]; hasErr {
		r.errSeen = true
		r.emit(payload)
		return
	}
	// 同型的「流内失败帧」：上游把安全策略拦截这类失败塞在顶层 {code,msg,displayMsg}、
	// 无 choices、无 error。若不认它，这帧会走完常规路径后什么都不命中（errSeen 不置位）
	// —— 于是 (a) 日志把它记成「段无 finish 结束（疑似上游断流）」，(b) 续写段还会白跑
	// 一次盲重试去撞同一个已被拦的账号。按显式失败处理：如实透传 + 终止续写。
	if code, msg, ok := upstream.StreamErrorFrame(obj); ok {
		log.Printf("WARN: [continue] 流内失败帧 code=%d msg=%q → 按显式失败终止（不续写）", code, msg)
		r.errSeen = true
		r.emit(payload)
		return
	}
	if id, ok := obj["id"].(string); ok && id != "" && r.id == "" {
		r.id = id
	}
	// usage 帧：消费入账（跨段合计），原始帧剥掉 usage 后再走常规路径——
	// 避免 usage 双发（本层末尾会合成一条总计帧），同时兼容与 finish/delta
	// 同帧的「bundled usage」形态（剥掉后内容照常透传）。
	hasUsage := false
	if u, ok := obj["usage"].(map[string]any); ok {
		r.mergeUsage(u)
		hasUsage = true
	}
	deltaNonEmpty := false
	rebuilt := false
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			if d, ok := c["delta"].(map[string]any); ok {
				if s, ok := d["reasoning_content"].(string); ok && s != "" {
					r.reason.WriteString(s)
					// 续写段思考的接缝策略（两个子情形）：
					//   - 正文尚未开始（r.text==0）：照常外泄。此时 responses 侧的
					//     reasoning item 还开着，续写思考只是往同一个 item 里续 delta，
					//     零协议风险；同时消掉「续写段纯思考期客户端零事件」的静默——
					//     codex 的 provider 带 stream_idle_timeout_ms，实测续写段静默
					//     约 122s 后客户端断开、请求记 interrupted（观测到的截断全是
					//     text=0B 的纯思考段，正是这条路径）。
					//   - 正文已开始（message item 开着）：剥离（照旧）。此时再喂思考
					//     responses 侧会开第二个 reasoning item，codex 后续文本 delta
					//     全报 "OutputTextDelta without active item"（实测 1.2 万条）。
					if r.seg > 0 && r.text.Len() > 0 {
						delete(d, "reasoning_content")
						rebuilt = true
					}
					deltaNonEmpty = true
				}
				if s, ok := d["content"].(string); ok && s != "" {
					r.text.WriteString(s)
					deltaNonEmpty = true
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					r.toolSeen = true
					deltaNonEmpty = true
				}
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				r.finish = fr
			}
		}
	}
	if hasUsage {
		delete(obj, "usage")
		rebuilt = true
	}
	if rebuilt {
		if raw, err := json.Marshal(obj); err == nil {
			payload = string(raw)
		}
	}
	// 空帧（各字段皆空的 no-op 帧、且非终态）→ 吞掉；这是残余的静默源，
	// 按需补 keepalive（chat 路径注释帧直达；codex 路径的续写思考帧已改为
	// 直通，见上面 reasoning 分支，不再走这里）。
	if r.seg > 0 && !deltaNonEmpty && r.finish == "" {
		r.maybeKeepalive()
		return
	}
	if r.finish != "" {
		// finish 已见：后续帧全部入 tail（续写成功则丢弃；否则原样回放）。
		r.tail = append(r.tail, payload)
		return
	}
	if hasUsage && !deltaNonEmpty {
		return // 纯 usage 帧（已入账）：不再下发
	}
	r.emit(payload)
}

func (r *continueReader) emit(payload string) {
	r.lastClientAt = time.Now()
	r.out = append(r.out, "data: "...)
	r.out = append(r.out, payload...)
	r.out = append(r.out, '\n', '\n')
}

// maybeKeepalive 被吞帧到达时调用：距上次客户端可见输出超过 keepaliveEvery 就
// 补一条 SSE 注释帧。注释行（":" 开头）是 SSE 规范的 no-op——StreamHint 原样
// 透传并 flush，responsesWriter 与 chat 类客户端的解析器都忽略它；唯一作用是
// 让连接保持「有数据流动」，防流静默超时掐线。
func (r *continueReader) maybeKeepalive() {
	if keepaliveEvery > 0 && time.Since(r.lastClientAt) < keepaliveEvery {
		return
	}
	r.out = append(r.out, ": keepalive\n\n"...)
	r.lastClientAt = time.Now()
}

// endSegment 一段结束（DONE 或 EOF/读错误）时的决策：
// 能续 → 换段继续读；否则回放 tail + 合成 usage 总计 + 结束。
func (r *continueReader) endSegment(sawDone bool) {
	if r.done {
		return
	}
	if r.cur != nil {
		r.cur.Close()
	}
	if r.canContinue(sawDone) {
		// 必须是「断流盲重试」还是「length 截断续写」（r.finish 在续写成功后被重置）。
		blind := !sawDone && r.finish == ""
		body, err := r.continuationBody()
		if err == nil {
			rc, status, _, terr := r.up.ChatStreamContext(r.ctx, r.acct, body, r.clientIP, r.meta)
			if terr == nil && status < 400 && rc != nil {
				r.seg++
				if blind {
					r.blindRetried = true
				}
				r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
				blindNote := ""
				if blind {
					blindNote = " (断流盲重试)"
				}
				log.Printf("[continue] seg=%d model=%s text=%dB reason=%dB%s -> 续写",
					r.seg, bareModelOf(r.body), r.text.Len(), r.reason.Len(), blindNote)
				r.finish, r.tail = "", nil
				return
			}
			if rc != nil {
				rc.Close()
			}
			log.Printf("WARN: [continue] seg=%d 续写失败（status=%d err=%v）→ 降级为截断终态", r.seg+1, status, terr)
		} else {
			log.Printf("WARN: [continue] 续写请求体构造失败: %v", err)
		}
	}
	// 段在没有任何 finish 的情况下以 EOF/读错误结束：疑似上游断流（正常收尾都带
	// finish_reason）。不断言语义，只记一条 WARN 供对账——这类流此前完全无痕，
	// 客户端却会看到「被标 completed 的中途截断」。
	if !sawDone && r.finish == "" && !r.errSeen {
		log.Printf("WARN: [continue] 段无 finish 结束（疑似上游断流，按截断终态降级）")
	}
	// 不续/续写失败：回放 tail（含 finish/末端帧，客户端行为与未接线时一致），
	// 再补一条跨段合计 usage，最后 EOF（[DONE] 由 StreamHint 统一补）。
	for _, f := range r.tail {
		r.emit(f)
	}
	r.tail = nil
	if r.usage != nil {
		raw, _ := json.Marshal(map[string]any{"id": r.id, "object": "chat.completion.chunk", "choices": []any{}, "usage": r.usage})
		r.emit(string(raw))
	}
	r.done = true
}

// canContinue 续写资格判定（sawDone=false 表示 EOF/读错误收尾——只有显式
// finish_reason=length 才续，读错误不续，避免把故障流当截断流续错）。
func (r *continueReader) canContinue(sawDone bool) bool {
	switch {
	case r.seg >= maxContinueSegments-1:
		return false
	case r.errSeen:
		return false
	case r.limitSet:
		return false
	case !sawDone && r.finish == "":
		// 段无 finish 结束 = 上游偶发断流（线上实测：15 次续写里 1 次）。续写段
		// 允许一次盲重试：重试请求体与断流段逐字节相同、已收内容都在 assistant
		// 前缀里，幂等不重复。主段不给盲重试（那是请求本身失败，交由既有降级）。
		return r.seg > 0 && !r.blindRetried
	case r.finish != "length":
		return false
	case r.toolSeen:
		log.Printf("[continue] 跳过：本段含工具调用分片（参数可能残缺，拼接风险高）")
		return false
	case r.text.Len() == 0 && r.reason.Len() == 0:
		return false
	}
	return true
}

// continuationBody 构造续写请求：原请求 messages + assistant（已输出内容）+ 续写指令。
// 复用原请求其余全部字段（model/tools/采样参数等）；max_tokens 类字段已在
// canContinue 阶段排除。prepareBody/thinking/缓存键等由 ChatStreamContext 内部统一套用。
func (r *continueReader) continuationBody() ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(r.body, &obj); err != nil {
		return nil, err
	}
	msgs, _ := obj["messages"].([]any)
	asst := map[string]any{"role": "assistant"}
	if t := r.text.String(); t != "" {
		asst["content"] = t
	} else {
		asst["content"] = ""
	}
	if rc := r.reason.String(); rc != "" {
		asst["reasoning_content"] = rc
	}
	msgs = append(msgs, asst, map[string]any{"role": "user", "content": continueNudge})
	obj["messages"] = msgs
	return json.Marshal(obj)
}

// mergeUsage 跨段累计 usage：数字叶子求和（token 计费口径），结构取并集。
func (r *continueReader) mergeUsage(u map[string]any) {
	if r.usage == nil {
		r.usage = map[string]any{}
	}
	sumInto(r.usage, u)
}

func sumInto(dst, src map[string]any) {
	for k, v := range src {
		switch sv := v.(type) {
		case float64:
			if dv, ok := dst[k].(float64); ok {
				dst[k] = dv + sv
			} else {
				dst[k] = sv
			}
		case map[string]any:
			if dm, ok := dst[k].(map[string]any); ok {
				sumInto(dm, sv)
			} else {
				nm := map[string]any{}
				sumInto(nm, sv)
				dst[k] = nm
			}
		default:
			dst[k] = v
		}
	}
}

// bareModelOf 从请求体取 model（仅日志用）。
func bareModelOf(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &obj) != nil {
		return "?"
	}
	return obj.Model
}
