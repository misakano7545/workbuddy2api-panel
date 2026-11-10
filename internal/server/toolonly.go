// toolonly.go 「说一句就停，但下一步本该动手」的安全续写。
//
// 与 narration.go 的分工：narration 救的是**尾部落在一句预告上**的形态（冒号、
// 省略号、未闭合代码块、无句末标点）；这里救的是**尾部看着说完了、其实没干完**的
// 形态——线上原样样本（17:29:45，acct=150****5392，27 个工具，prompt 106,507）：
//
//	"这是免注册即可变更他人登记信息的接口：只靠邮箱就能改。这是高价值面，
//	 需要走通验证。先看完整表单。"
//
// finish_reason=stop、零 tool_calls、零报错。模型认为这轮说完了，客户端回合就此结束，
// 用户看到的就是「说到一半没了」。取证 156 条同类样本里，这种句号收尾占 38 条。
//
// 为什么不能像 narration 那样直接把续写正文接上去：句号收尾的样本里既有真正的
// 「马上要动手」，也有大量正常短答（角色扮演闲聊、明确在等用户回话、拒绝回答）。
// 接正文 = 把正常回答续成一段废话。
//
// 所以这里的采用条件是**只认工具调用**：
//   - 续写段拿到 tool_calls → 采纳（客户端立刻就能继续执行下一步）；
//   - 续写段只有正文 → 整段丢弃，客户端看到的内容与接线前**一模一样**。
//
// 判据刻意压窄，且误命中的代价只有「多打一次上游」（前缀缓存命中，费用接近 0）：
//  1. finish=stop、正文非空、零 tool_calls、无 error 帧；
//  2. 请求声明了工具，历史里有工具轮，**且最后一条消息就是工具结果**——模型刚看完
//     工具输出，本来该继续动手；闲聊/角色扮演的最后一跳通常是 user 或 assistant；
//  3. 正文很短（一眼扫过的一句话，< toolOnlyMaxBytes）；
//  4. 尾部不落在预告符上（那种由 narration 救，避免同一请求救两次）；
//  5. prompt 够长（> toolOnlyMinPrompt，长会话里的短输出才是异常；不知道就不救）；
//  6. 每请求一次。
package server

import (
	"bufio"
	"encoding/json"
	"log"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
)

// toolOnlyMaxBytes 首段正文超过这个长度就不算「一次提前收尾」。
//
// 取值依据（两台机器实测，2026-11-10）：
//   - 预告句收尾那批（"…："/无标点）正文多集中在 40~200B；
//   - 「写完一段就收尾」要长得多：中断投诉对得上的两条是 completion 461 / 316 token
//     （≈1400B / 950B），句号收尾、零工具调用；
//   - 按 25 分钟样本放宽到 N token 的覆盖率（prompt≥8k 那批 46 条）：
//     ≤650(2000B) 41% / ≤1000 76% / ≤2500 87% / 不设限 100%。
// 上限取 2000B。曾试过放到 7500B（覆盖 41%→77%），但上线后出现反向副作用：
// 已经写完的任务被推着再调一次工具（用户反馈「任务该结束了却一直在工具调用」），
// 因为「模型肯再调一次」并不等于「它还没做完」。在没有可靠判据区分「提前收尾」与
// 「已经答完」之前，宁可少救：救援只在正文明显短（一句预告）时才触发。
const toolOnlyMaxBytes = 2000

// toolOnlyMinPrompt 长会话门槛：几百 token 的短问答里，模型给一句就收尾是正常的。
// 实测「被门槛挡掉」的那批都是 4k~10k prompt（AsyncOpenAI / Go-http-client 的小会话），
// 给到 8000 能覆盖它们，又不会把一句话问答也算进来。
const toolOnlyMinPrompt = 20000

// toolOnlyNudge 安全续写的指令。必须点明「别再用文字解释」——否则模型会再写一段
// 总结，那段正文会被丢弃，等于白发一次请求；也必须点明「不要重复上一个工具调用」，
// 否则一个以为「再来一次就好」的模型会把刚执行完的工具原样再跑一遍（写操作会二次生效）。
const toolOnlyNudge = "继续执行下一步。需要查看文件、执行命令或使用工具时，直接调用工具，不要再用文字解释或重复已有结论。不要重复调用刚才已经执行过的同一个工具、也不要重发相同的参数。如果确实已经全部完成，请只回复 DONE。"

// lastMessageIsToolResult 请求体的最后一条消息是否工具结果。
//
// 这是本判据里最强的结构信号：agent 循环里每次工具执行完都会带着「最后一条是
// tool 消息」再请求下一次；模型此时只写一句话就停，基本就是没接着动手。
// 闲聊/角色扮演的最后一跳是 user 或 assistant，不会命中。
func lastMessageIsToolResult(body []byte) bool {
	var obj struct {
		Msgs []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Msgs) == 0 {
		return false
	}
	last := obj.Msgs[len(obj.Msgs)-1]
	return last.Role == "tool" || last.ToolCallID != ""
}

// tryToolOnlyRescue 安全续写：只采纳工具调用，正文一律丢弃。
func (r *continueReader) tryToolOnlyRescue() bool {
	switch {
	case r.toolOnlyTried, r.narrationTried, r.errSeen, r.toolSeen, r.clientGone(), r.outputCapped():
		return false
	case r.finish != "stop":
		return false
	case r.text.Len() == 0 || r.text.Len() > toolOnlyMaxBytes:
		return false
	case r.seg >= maxContinueSegments-1:
		return false
	}
	// 正文里含工具调用标记（DSML / XML 形态）：客户端会自己解析执行，这不是「提前收尾」。
	if hasToolCallMarkup(r.text.String()) {
		return false
	}
	// 尾部落在预告符上：交给 narration（那条会把正文接上去，比这里更合适）。
	if narrationTailSuspends(r.text.String()) {
		return false
	}
	declared, toolTurn := bodyToolShape(r.body)
	if !declared || !toolTurn || !lastMessageIsToolResult(r.body) {
		return false
	}
	// prompt 未知（上游没给 usage）就不救：无法区分长会话与一句问答。
	if p, ok := r.usage["prompt_tokens"].(float64); !ok || p < toolOnlyMinPrompt {
		return false
	}
	body, err := r.continuationBodyWithNudge(toolOnlyNudge)
	if err != nil {
		return false
	}
	r.toolOnlyTried = true
	// 首段的终态帧先存起来：续写段如果只给正文，要原样回放这一份。
	r.origFinish, r.origTail = r.finish, r.tail
	rc, status, _, terr := r.up.ChatStreamContext(r.ctx, r.acct, body, r.clientIP, r.meta, nil)
	if terr != nil || status >= 400 || rc == nil {
		if rc != nil {
			rc.Close()
		}
		r.origFinish, r.origTail = "", nil
		log.Printf("WARN: [toolonly] acct=%s seg=%d 续写请求失败（status=%d err=%v）→ 按原样收尾",
			logfmt.Label(r.acct.UID, r.acct.Nickname), r.seg+1, status, terr)
		return false
	}
	r.seg++
	r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
	r.finish, r.tail = "", nil
	r.toolOnly = true
	r.segCompletion, r.segThink = 0, 0
	log.Printf("[toolonly] acct=%s model=%s seg=%d 正文只有 %dB 就收尾（末条是工具结果）→ 试续一次，只采纳工具调用",
		logfmt.Label(r.acct.UID, r.acct.Nickname), bareModelOf(r.body), r.seg, r.text.Len())
	return true
}

// settleToolOnly 续写段结束时的裁决：拿到工具调用就采纳，否则整段回滚。
// 返回 true 表示已回滚（调用方继续走正常收尾路径，回放首段的终态帧）。
func (r *continueReader) settleToolOnly() bool {
	if !r.toolOnly {
		return false
	}
	r.toolOnly = false
	if r.toolSeen {
		// 采纳：续写段的 tool_calls 已经实时发给客户端了，收尾照常（回放本段 tail）。
		r.origFinish, r.origTail = "", nil
		log.Printf("[toolonly] acct=%s model=%s seg=%d 续写段拿到工具调用 → 采纳",
			logfmt.Label(r.acct.UID, r.acct.Nickname), bareModelOf(r.body), r.seg)
		return false
	}
	// 回滚：续写段只有正文（或什么都没有）→ 丢掉，客户端看到的内容与接线前一致。
	r.finish, r.tail = r.origFinish, r.origTail
	r.origFinish, r.origTail = "", nil
	log.Printf("[toolonly] acct=%s model=%s seg=%d 续写段没有工具调用（正文 %dB）→ 丢弃，按原样收尾",
		logfmt.Label(r.acct.UID, r.acct.Nickname), bareModelOf(r.body), r.seg, r.text.Len())
	return true
}

// discardToolOnlyDelta 在「只采纳工具调用」阶段剥掉正文/思考增量：这些字节不给客户端，
// 也不进本地累积（否则后续续写前缀里会混进被丢弃的内容）。
// 返回 true 表示该 delta 被改动过，需要重新序列化。
func discardToolOnlyDelta(d map[string]any) bool {
	changed := false
	if s, ok := d["content"].(string); ok && s != "" {
		delete(d, "content")
		changed = true
	}
	if s, ok := d["reasoning_content"].(string); ok && s != "" {
		delete(d, "reasoning_content")
		changed = true
	}
	return changed
}

// trimForLog 截断日志里的正文片段。
func trimForLog(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:] + "…" // 取尾部：收尾形态才是判据关心的
}

// tailOf 取字符串尾部 n 字节（按 rune 边界回退，避免截断半个汉字）。
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && (s[i]&0xC0) == 0x80 {
		i++
	}
	return s[i:]
}
