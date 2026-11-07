// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// adoptReportGap 领养前置上报后的等待：给上游事件处理留时间再发 buddy/first。
// 对齐 scripts/task_first_buddy.py 实测的 1.05s 间隔口径。测试可置 0。
var adoptReportGap = 1050 * time.Millisecond

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return cst.Day(t)
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检。
// 禁用账号跳过；**暂停选号（paused）账号照常参与**——旅行是纯 RPC（状态 / 派出 /
// 领奖 + 领养前置上报），不发模型对话，与「让位防风控」不冲突（吸收上游 b1a2284；
// 别顺手把 paused 加进来，那是被上游撤回过的一揽子跳过）；
// 401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay。
func (s *Scheduler) RunTravelNow() {
	s.runTravel(context.Background(), false)
}

// runTravel 旅行巡检遍历。spread=true（排程路径）时按账号级偏移错开，见 spread.go。
func (s *Scheduler) runTravel(ctx context.Context, spread bool) {
	plan := s.spreadPlan(taskTravel, spread, cst.Now())
	first := true
	for _, st := range plan.order(s.cfg.Pool.List()) {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		if !a.SupportsPersonalGrowth() {
			continue // 企业版 / 静态 API Key 门控：无个人成长体系，不发起任何上游调用
		}
		if !plan.wait(ctx, st.UID) {
			return // ctx 取消：剩余账号下轮再巡
		}
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		s.travelOne(a)
	}
}

// RunTravelNowFor 只对指定账号执行一趟旅行巡检（按号端点用），门控与全池版本一致。
// 返回是否真的推进了：false = 该号被门控跳过（禁用/暂停/无 refresh token/global/企业）。
//
// shortcut: 不加账号间限速，调用方自己控节奏——按号端点存在的意义就是让外部
// 调度器做到每号独立随机时点，再叠加固定节拍等于把指纹加回去。
func (s *Scheduler) RunTravelNowFor(uid string) bool {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok || st.Disabled {
		return false
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshTokenValue() == "" {
		return false
	}
	if a.IsGlobal() || a.IsEnterprise() {
		return false
	}
	s.travelOne(a)
	return true
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
func (s *Scheduler) travelOne(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("WARN: travel %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if buddy == nil {
		s.travelAdopt(a)
		return
	}
	// 有猫才有盲盒入口（盲盒开的是猫的道具，无猫的号连配额接口都没有意义）。
	// 放在状态机之前：与派出/领奖互不影响，早开早省得撞上游时段限流。
	s.openBlindBox(a)
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("WARN: travel %s: status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	switch ts.State {
	case travelStateArrived:
		s.travelClaim(a, ts)
	case travelStateIdle:
		s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.Label(a.UID, a.Nickname), ts.RecordID)
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.Label(a.UID, a.Nickname), ts.State)
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(a.UID, a.Nickname))
		return
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("WARN: travel %s: depart: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.Label(a.UID, a.Nickname), travelLocationID)
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.Label(a.UID, a.Nickname))
		return
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("WARN: travel %s: claim record=%d: %v", logfmt.Label(a.UID, a.Nickname), ts.RecordID, err)
		return
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.Label(a.UID, a.Nickname), ts.RecordID, reward)
}

// travelAdopt 无猫时领养，链路：report → agreement → buddy/first。
//
// report 必须先跑（scripts/task_first_buddy.py 实测）：一条 chat_request_send 上报
// 点亮 growth 连登并**解锁 first_buddy 任务**；未上报时 buddy/first 会返回
// 400 "first_buddy task not completed yet"——该门槛的真实来源是"当日无活跃上报"，
// 不是账号问题（report.go 注释亦明确「解锁 first_buddy 任务（领养前置）」）。
// conversation 门槛未达标仍属预期行为，记一次当日已试后静默跳过，不再重试。
func (s *Scheduler) travelAdopt(a *auth.Auth) {
	if s.adoptTriedToday(a.UID) {
		return
	}
	// 前置：解锁 first_buddy 任务（幂等；失败不阻塞，让 buddy/first 按既有错误路径暴露）。
	// 会话 ID 用官方形态（v4 UUID）。旧值是 `wb2api-adopt-<毫秒>`——项目缩写 +
	// 时间戳直接进上报体（issue #51）。
	if err := s.cfg.Upstream.ReportChatActivity(a, session.NewConversationID(), ""); err != nil {
		log.Printf("WARN: travel %s: adopt preflight report: %v", logfmt.Label(a.UID, a.Nickname), err)
	} else {
		time.Sleep(adoptReportGap) // 给上游事件处理留时间（对齐脚本实测的 1.05s 间隔口径）
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("WARN: travel %s: agreement: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(a.UID, a.Nickname))
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.Label(a.UID, a.Nickname))
	default:
		log.Printf("WARN: travel %s: adopt: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
}

// blindBoxMaxOpens 单轮最多开几个盲盒：一次扣 10 能量，一轮把存下的能量全花光
// 既没必要也更容易撞风控；剩下的留给下一个旅行时点。
const blindBoxMaxOpens = 5

// openBlindBox 有能量就开盲盒（任务奖励发的 energy 到这里才第一次被消费）。
//
// 顺序：挂在旅行巡检同一条腿上、每号旅行动作之后跑。能量由任务在 growth 时点
// （默认 11 点）结算，而旅行时点是 9/21 点——21 点这轮读到的已是当日结算后的
// 余额；配额每次都现查，不缓存，所以不存在「今天发的能量还没到账就判能量不足」。
func (s *Scheduler) openBlindBox(a *auth.Auth) {
	q, err := s.cfg.Upstream.BuddyQuota(a)
	if err != nil {
		log.Printf("WARN: blindbox %s: quota: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if q == nil || q.Affordable <= 0 {
		balance := 0
		if q != nil {
			balance = q.Balance
		}
		log.Printf("blindbox %s: skip (energy %d < 10)", logfmt.Label(a.UID, a.Nickname), balance)
		return
	}
	n := q.Affordable
	if n > blindBoxMaxOpens {
		n = blindBoxMaxOpens
	}
	got := make([]string, 0, n)
	for i := 0; i < n; i++ {
		item, err := s.cfg.Upstream.BuddyOpen(a)
		if err != nil {
			log.Printf("WARN: blindbox %s: open %d/%d: %v", logfmt.Label(a.UID, a.Nickname), i+1, n, err)
			break
		}
		got = append(got, item.String())
	}
	if len(got) == 0 {
		return
	}
	log.Printf("blindbox %s: open %d/%d (%s)", logfmt.Label(a.UID, a.Nickname), len(got), n, strings.Join(got, "、"))
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
