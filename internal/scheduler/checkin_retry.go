package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// checkinRetryDelays 瞬时失败后的延后补签节奏：从本批结束起算的**绝对偏移**。
//
// 为什么需要独立于 DailyCheckin 的内联重试：后者只等 2s/4s（retryBillingTransient），
// 而线上实测的上游形态是 500 + code 10001「签到失败，请稍后重试」——这类要求「稍后」
// 的失败需要分钟级沉降，6 秒内三次全败，之后要等 21:00 的下一轮排程（12 小时后），
// 中间这段时间账号当天的签到就是没有的（漏一天就是断连登）。
//
// 为什么只有两轮：21:00 的正式排程是天然的第二次机会，再加轮次收益递减、而每轮都是
// 真实写调用。10 分钟覆盖「刚结算完的抖动」，40 分钟覆盖「上游处理慢」，两轮之后仍失败
// 基本是账号或上游的系统性问题，靠重试解决不了，落 WARN 让运维看见。
//
// 变量而非常量：测试要把它压到毫秒级，避免真等 40 分钟。
var checkinRetryDelays = []time.Duration{10 * time.Minute, 40 * time.Minute}

// checkinOne 对单个 CN 账号执行签到（不含余额查询）。返回 done 表示今天确实已签
// （成功、上游显示已签、幂等拒绝三种都算），err 是真实失败原因供调用方判断可否重试。
//
// 抽出来是为了让「排程主循环」与「延后补签」共用同一份判据——两处各写一遍必然漂移，
// 而漂移的表现就是补签把幂等拒绝当成失败、或者把真失败当成已签。
func (s *Scheduler) checkinOne(a *auth.Auth, uid string) (bool, error) {
	label := logfmt.Label(uid, a.Nickname)
	// 先查活动状态（只读）：本机 checkin_done 只记录我们自己发起的签到，用户从手机 /
	// 官方客户端签过就看不到；上游 today_checked_in 才是权威事实。已签则直接记账跳过
	// （省掉 21:00 那一轮注定被幂等拒绝的写调用），顺带把连登与活动截止落进日志；
	// active=false 说明活动已结束/未开始，提前显形。**查询失败 fail-open**——照常签到
	// （签到自身的幂等错误标记仍是兜底判据）。
	act, actErr := s.cfg.Upstream.GetCheckinActivityStatus(a)
	switch {
	case actErr != nil:
		log.Printf("WARN: checkin %s: 活动状态查询失败（照常签到）: %v", label, actErr)
	case !act.Active:
		log.Printf("WARN: checkin %s: 上游签到活动当前不生效（截止 %s），仍尝试签到", label, act.EndTime)
	}
	if actErr == nil && act.TodayCheckedIn {
		s.cfg.Pool.NoteCheckinDone(uid)
		log.Printf("checkin %s: 上游显示今天已签到（连登 %d 天，活动至 %s）", label, act.StreakDays, act.EndTime)
		return true, nil
	}
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
		if upstream.IsAlreadyCheckin(err) {
			s.cfg.Pool.NoteCheckinDone(uid)
			log.Printf("checkin %s: 今天已签到（幂等）", label)
			return true, nil
		}
		log.Printf("WARN: checkin %s: %v", label, err)
		return false, err
	}
	// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在重复触发时
	// 出现），成功也落一行。
	s.cfg.Pool.NoteCheckinDone(uid)
	log.Printf("checkin %s: 签到成功", label)
	return true, nil
}

// startCheckinRetry 把本轮瞬时失败的账号排进后台补签队列。
//
// 不阻塞调用方（runCheckin 在排程 goroutine 里跑，摊开启用时 spreadRunning 标记要
// 等它返回才清，阻塞会让下一个时点整批被跳过）；ctx 取消时立即退出，不拖住优雅停机。
func (s *Scheduler) startCheckinRetry(ctx context.Context, uids []string) {
	if len(uids) == 0 {
		return
	}
	pending := append([]string(nil), uids...)
	log.Printf("[checkin] %d 个账号本轮瞬时失败，将在 %s、%s 后各补签一次（期间 21:00 排程照常）",
		len(pending), checkinRetryDelays[0], checkinRetryDelays[len(checkinRetryDelays)-1])
	go s.checkinRetryLoop(ctx, pending)
}

func (s *Scheduler) checkinRetryLoop(ctx context.Context, pending []string) {
	prev := time.Duration(0)
	for round := 0; round < len(checkinRetryDelays); round++ {
		// 偏移是绝对的（10m、40m），所以这里睡的是与上一轮的差值。
		if !sleepCtx(ctx, checkinRetryDelays[round]-prev) {
			return // ctx 取消：停机，不补
		}
		prev = checkinRetryDelays[round]

		var still []string
		for _, uid := range pending {
			if ctx.Err() != nil {
				return
			}
			// 已签（含用户在手机上签了、或上一轮补签成功）就不再打上游。
			if s.cfg.Pool.CheckinDoneToday(uid) {
				continue
			}
			a := s.cfg.Pool.AuthByUID(uid)
			if a == nil || a.IsGlobal() || !a.SupportsPersonalEndpoints() {
				continue
			}
			// 与主循环同一条禁用规则：禁用只关选号、不停保号，但要看开关。
			if st, ok := s.cfg.Pool.Status(uid); ok && st.Disabled && !s.includeDisabledInTasks() {
				continue
			}
			done, err := s.checkinOne(a, uid)
			switch {
			case done:
				log.Printf("[checkin] 补签成功 %s（第 %d 轮 / 共 %d 轮）",
					logfmt.Label(uid, a.Nickname), round+1, len(checkinRetryDelays))
			case err != nil && upstream.IsTransientBilling(err):
				still = append(still, uid)
			default:
				// 非瞬时错误（业务拒绝等）：重试不会有不同结果，本轮就放弃它。
				log.Printf("[checkin] %s 补签遇到不可重试的错误，放弃补签（%v）",
					logfmt.Label(uid, a.Nickname), err)
			}
		}
		pending = still
		if len(pending) == 0 {
			return
		}
	}
	log.Printf("WARN: [checkin] 补签 %d 轮后仍有 %d 个账号失败（等 21:00 排程再试）：%s",
		len(checkinRetryDelays), len(pending), joinUIDs(pending))
}

// joinUIDs 只打 uid 前 8 位：日志里出现完整 uuid 没有额外信息，短前缀与面板一致。
func joinUIDs(uids []string) string {
	out := ""
	for i, u := range uids {
		if i > 0 {
			out += ", "
		}
		if len(u) > 8 {
			u = u[:8]
		}
		out += u
	}
	return out
}
