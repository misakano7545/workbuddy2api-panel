// windowprobe.go 额度窗口探针（借鉴 ardeyouxipianyi/workbuddy2api-hub 的 wb_window_probe.py）。
//
// 登记侧在 pool/window.go：429 带「将在 … 重置」时记一条窗口。本文件是执行侧——
// 到点后为那个 (账号, 模型) 发一个最小请求，把新一轮配额窗口的计时起点钉在重置
// 那一刻。
//
// 为什么必须主动发：上游按**首次请求**起算窗口。重置时刻到了却没人发请求，窗口就
// 不开始，额度一直空转；等下一条真实请求进来，起点落到那一刻——通常晚了几分钟到
// 几十分钟，那段时间的额度等于白扔。我们原来的处理只到「解除冷却」（允许请求进来），
// 是被动的。
//
// 四种结果的处置：
//   - 探针 2xx         → 窗口已开：清掉该 (账号,模型) 的 6004 冷却，删窗口记录；
//   - 429 + 更晚的重置 → 上游确实开了新一轮，把 ResetAt 换成新的，计数归零；
//   - 429 + 同样/更早   → 上游还没放开（服务端时钟/处理延迟），过一会儿重试；
//   - 其它错误         → 同样按重试处理，连续 windowProbeMaxAttempts 次后放弃
//     （删记录，下一次真实 429 会重新登记）。
//
// 刻意**不**做的事：主动扫描还没被限流的「账号 × 模型」配额线（hub 的
// wb_quota_scan）。那是另一种功能：它要用探针去**发现**哪条线满了，请求量随
// 账号×模型数增长，与「已经知道满了、只等起窗」不是一件事。混在一起会让本机制的
// 请求量从「每个已登记窗口一次」变成「每 6 小时 × 账号数 × 模型数」。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strconv"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

const (
	// windowProbePrompt / windowProbeMaxTokens 探针请求要尽量小：它唯一的目的是让
	// 上游把这一轮窗口的计时起点定在此刻，产出什么都不重要。
	windowProbePrompt    = "Hi"
	windowProbeMaxTokens = 16
	// windowProbeTimeout 单次探针的硬上限。取 60s：上游劣化期 TTFB 可能到几十秒
	// （见 internal/server/upstreamhealth.go 的实测），但探针不值得为它等更久。
	windowProbeTimeout = 60 * time.Second
	// windowProbeRetryDelay 到点后上游常还没真正放开（服务端时钟/处理延迟几秒到
	// 几分钟）。15s 首次重试：太密是白打上游，太疏会错过窗口起点。
	windowProbeRetryDelay = 15 * time.Second
	// windowProbeMaxAttempts 连续这么多次探针仍未开窗就放弃。20 × 15s ≈ 5 分钟，
	// 覆盖「上游声明时刻比实际早几分钟」的常见偏差。
	windowProbeMaxAttempts = 20
	// windowProbeFreshMin 429 给的重置点比「现在 + 这个值」还远，才算上游真的开了
	// 一轮新窗口；否则只是探针发早了，仍按同一轮重试。
	windowProbeFreshMin = 30 * time.Second
	// windowProbeBatchLimit 一轮最多处理几个窗口：一个卡住的上游不该把巡检线程占满。
	windowProbeBatchLimit = 8
	// windowProbeGap 多个窗口同时到点时错开，别在同一秒向上游打一串请求。
	windowProbeGap = 400 * time.Millisecond
	// windowProbeIdle 没有待处理窗口时的巡检间隔。有窗口时按最近的 ResetAt 精确
	// 醒来（见 Run 循环），这个值只在完全空闲时用于兜底——比如面板手工登记、
	// 或另一个进程写了 state.json。
	windowProbeIdle = 30 * time.Second
)

// windowProbeBody 构造探针请求体。
//
// stream:true 是必须的（上游只认流式，非流式会 400/挂住，同 /v1/chat/completions
// 的 stream:false 路径也是按 SSE 聚合出来的）。
func windowProbeBody(model string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": windowProbePrompt},
		},
		"stream":     true,
		"max_tokens": windowProbeMaxTokens,
	})
}

// StartModelWindowProbe 启动额度窗口探针循环（独立 goroutine，ctx 取消即停）。
//
// 与 StartBalanceRefresh 同构：独立于 Run 的小时制排程——窗口到点是秒级事件，
// 不值得为它扩展 nextFire 的粒度。
func (s *Scheduler) StartModelWindowProbe(ctx context.Context) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	go func() {
		for {
			due := s.cfg.Pool.DueModelWindows(time.Now())
			if len(due) > 0 {
				s.runWindowProbes(ctx, due)
				if ctx.Err() != nil {
					return
				}
				// 一轮处理完立刻回到循环顶重判（可能还有待处理窗口）。
				continue
			}
			wait := windowProbeIdle
			if next, ok := s.cfg.Pool.NextModelWindowDue(); ok {
				if d := time.Until(next); d < wait {
					wait = d
				}
			}
			if wait < 0 {
				wait = 0
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// RunModelWindowProbeNow 立即处理一轮到点的窗口，返回实际发出的探针数。
// 面板「立即探针」与测试用；正常路径由 StartModelWindowProbe 驱动。
func (s *Scheduler) RunModelWindowProbeNow() int {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return 0
	}
	return s.runWindowProbes(context.Background(), s.cfg.Pool.DueModelWindows(time.Now()))
}

// runWindowProbes 逐条处理窗口。返回实际发出的探针数（不含被跳过的）。
func (s *Scheduler) runWindowProbes(ctx context.Context, due []pool.ModelWindow) int {
	if len(due) > windowProbeBatchLimit {
		due = due[:windowProbeBatchLimit]
	}
	sent := 0
	for i, w := range due {
		if ctx.Err() != nil {
			return sent
		}
		if i > 0 {
			// 可取消的错开等待：ctx 取消（停机）时不必等满。
			select {
			case <-ctx.Done():
				return sent
			case <-time.After(windowProbeGap):
			}
		}
		if s.probeWindow(ctx, w) {
			sent++
		}
	}
	return sent
}

// probeWindow 处理单个窗口，返回是否真的发出了上游请求。
func (s *Scheduler) probeWindow(ctx context.Context, w pool.ModelWindow) bool {
	a := s.cfg.Pool.AuthFor(w.UID)
	now := time.Now()
	if a == nil {
		// 账号已不在池中（被删/被替换）：记录留着只会每轮都被取出来，删掉。
		s.cfg.Pool.DropModelWindow(w.UID, w.Model)
		log.Printf("INFO: [window] 账号 %s 已不在池中，丢弃 %s 的窗口记录", shortUID(w.UID), w.Model)
		return false
	}
	st, ok := s.cfg.Pool.Status(w.UID)
	if ok && st.Disabled {
		// 停用的号不该被自动唤醒：把 ResetAt 往后推一段，保留记录等它被重新启用。
		s.cfg.Pool.RecordWindowProbe(w.UID, w.Model, now.Add(windowProbeRetryDelay),
			w.Attempts+1, "账号已停用，暂不探针", now)
		return false
	}

	ok2, resetAt, detail := s.fireWindowProbe(ctx, w)
	now = time.Now()
	if ok2 {
		// 探针通了：这条额度线已经重置。清 6004 冷却 + 删窗口记录。
		//
		// 清冷却的对齐点是**这条窗口自己的 ResetAt**，不是现在：探针证明的是「这条
		// 窗口对应的那轮限额已经过去」，只有 ResetAt 不晚于它的冷却才该被清。探针
		// 期间若又撞上更新一轮的限额（ResetAt 更晚），那条是上游刚给的墙钟，不能被
		// 这次成功抹掉。
		cleared := s.cfg.Pool.ClearQuotaCooldown(w.UID, w.Model, w.ResetAt)
		s.cfg.Pool.DropModelWindow(w.UID, w.Model)
		log.Printf("INFO: [window] %s 的 %s 已开始新一轮计时（探针成功，清冷却=%v）",
			shortUID(w.UID), w.Model, cleared)
		return true
	}
	if !resetAt.IsZero() && resetAt.After(now.Add(windowProbeFreshMin)) {
		// 上游给了更晚的重置点：这条线确实进入了新的等待期，换成新墙钟、计数归零。
		s.cfg.Pool.RecordWindowProbe(w.UID, w.Model, resetAt, 0, detail, now)
		log.Printf("INFO: [window] %s 的 %s 仍在限流，下一轮重置 %s",
			shortUID(w.UID), w.Model, resetAt.Format("2006-01-02 15:04:05"))
		return true
	}
	attempts := w.Attempts + 1
	if attempts >= windowProbeMaxAttempts {
		s.cfg.Pool.DropModelWindow(w.UID, w.Model)
		log.Printf("WARN: [window] %s 的 %s 连续 %d 次未重置，放弃（下一次真实 429 会重新登记）",
			shortUID(w.UID), w.Model, attempts)
		return true
	}
	s.cfg.Pool.RecordWindowProbe(w.UID, w.Model, now.Add(windowProbeRetryDelay),
		attempts, detail, now)
	log.Printf("INFO: [window] %s 的 %s 尚未重置（%s），%s 后重试 (%d/%d)",
		shortUID(w.UID), w.Model, orNone(detail), windowProbeRetryDelay, attempts, windowProbeMaxAttempts)
	return true
}

// fireWindowProbe 发一次最小请求。
//
// 返回 (ok, resetAt, detail)：
//   - ok=true       上游 2xx，该模型的额度窗口已重置；
//   - resetAt 非零  上游又回了 429 并给出重置时刻；
//   - detail        失败原因（日志与窗口记录用）。
func (s *Scheduler) fireWindowProbe(ctx context.Context, w pool.ModelWindow) (bool, time.Time, string) {
	a := s.cfg.Pool.AuthFor(w.UID)
	if a == nil {
		return false, time.Time{}, "账号已不在池中"
	}
	body, err := windowProbeBody(w.Model)
	if err != nil {
		return false, time.Time{}, "构造请求失败: " + err.Error()
	}
	pctx, cancel := context.WithTimeout(ctx, windowProbeTimeout)
	defer cancel()
	// AgentType=auxiliary：这是一次内部维护请求，不是用户主对话（口径同
	// web_search 的探测轮）。上游看到的仍是一个正常的 chat 请求。
	rc, status, respBody, err := s.cfg.Upstream.ChatStreamContext(pctx, a, body, "",
		upstream.ChatMeta{AgentType: upstream.AgentTypeAuxiliary}, nil)
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) {
			// body 的重置文案优先（上游权威），其次看状态码。
			if resetAt, ok := upstream.ParseRateReset(ue.Msg); ok {
				return false, resetAt, clip(ue.Msg)
			}
			if ue.Status == 429 {
				return false, time.Time{}, clip(ue.Msg)
			}
			return false, time.Time{}, "HTTP " + strconv.Itoa(ue.Status) + ": " + clip(ue.Msg)
		}
		return false, time.Time{}, clip(err.Error())
	}
	if rc != nil {
		// 读干净再关：不读完的响应体不归还连接（net/http 语义），而这条连接后面
		// 还要被真实请求复用。max_tokens=16 时这一步是毫秒级。
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if status >= 400 {
		if resetAt, ok := upstream.ParseRateReset(string(respBody)); ok {
			return false, resetAt, clip(string(respBody))
		}
		return false, time.Time{}, "HTTP " + strconv.Itoa(status) + ": " + clip(string(respBody))
	}
	return true, time.Time{}, ""
}

// shortUID 日志里的账号称呼：前 8 位，不落完整 UID（同 dump.go 口径）。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	if uid == "" {
		return "?"
	}
	return uid
}

func orNone(s string) string {
	if s == "" {
		return "无详情"
	}
	return s
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
