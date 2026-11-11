// tasksweep.go 补领清扫：只 accept 待接受的、只 claim 已达标未领的，不跑任何行为链路。
//
// 动机（issue #174）：达标未领的任务此前没有补领兜底——新号自动跑时它们还没达标
// （awaiting_progress），成长队列一天只跑一次，白名单（autoActions）外的码更是永远
// 进不了队列。本清扫与 autoActions 无关，判据只有 progress，故周期跑即可兜住全部漏领。
package panel

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// RunTaskSweepNow 一轮清扫（scheduler 周期调用）。返回 (清扫账号数, 摘要)。
// 已禁用 / 国际区 / 企业版账号一个上游请求都不发（门控与任务中心扫描一致）；
// 正在跑任务动作的账号本轮让位（与单任务 / 一键完成 / 成长队列共用 per-account 锁）。
func (p *Panel) RunTaskSweepNow() (int, string) {
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return 0, "账号池未接线"
	}
	var swept, busy, accepted, claimed, failed int
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled || st.Realm == "global" {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || !a.SupportsPersonalGrowth() {
			continue // a==nil 无凭证；企业版 / 静态 API Key 无个人成长体系
		}
		if !p.tryLockAccount(st.UID) {
			busy++
			continue
		}
		acc, cl, err := p.sweepAccount(a)
		p.unlockAccount(st.UID)
		swept++
		accepted += acc
		claimed += cl
		if err != nil {
			failed++
			log.Printf("WARN: 补领清扫 %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		}
	}
	return swept, fmt.Sprintf("清扫 %d 个账号（跳过 %d）· 接受 %d · 领奖 %d · 出错 %d",
		swept, busy, accepted, claimed, failed)
}

// sweepAccount 单账号清扫，返回 (接受数, 领奖数, 列表失败错误)。
//
// 领奖判据只用 progress（Task.Claimable）而不是 accept_status：后者会在回读时短暂转
// completed 随后被整体回滚（见 autotask.go），据此领奖会误领。领奖本身幂等（重复领
// 上游返回 already_claimed → 0 分 0 能），所以逐条失败只记 WARN 不中断，宁可多试一轮。
//
// 接受走 acceptPendingWithFallback（与一键完成共用同一份补偿逻辑）：清洁路径仍是
// 「一次批量 accept」，与改动前请求序列逐字一致；只有批量整体失败时才回读列表并对
// 仍未生效的码逐码重试。
func (p *Panel) sweepAccount(a *auth.Auth) (accepted, claimed int, err error) {
	label := logfmt.Label(a.UID, a.Nickname)
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0, 0, err
	}
	mp, mpErr := p.cfg.Upstream.ListTasksMP(a)
	if mpErr != nil {
		log.Printf("WARN: 补领清扫 %s mp 列表: %v", label, mpErr)
	}

	// 端标记不同，两种口径分开提交（缺 mp 头 accept 返回 task not found）。
	//
	// 为什么不再直接调 AcceptTasks / AcceptTasksMP 并在失败时只记 WARN：批量接受是
	// 「一坏全坏」——上游对整批只回一个 error，其中任一码下架 / 前置不满足 / 撞频控，
	// 其余本来能接受的码全部停在 not_accepted。而本清扫正是「任务已达标但用户没拿到
	// 奖励」的唯一兜底路径，一坏全坏在这里的表现就是「任务做完却永远不自动领积分」。
	// 补偿逻辑（回读 accept_status → 逐码重试 → 上限 20 → reportGap 节流）与一键完成
	// 共用同一个 acceptPendingWithFallback，不另写一份。
	//
	// RunTaskSweepNow 没有 ctx（scheduler 周期触发，取消语义是「关进程」），传
	// context.Background()：补偿循环里 ctx.Err() 恒为 nil，靠 20 个上限收敛。
	if codes := acceptPendingCodes(tasks); len(codes) > 0 {
		n, notes := p.acceptPendingWithFallback(context.Background(), a, codes, false)
		accepted += n
		if len(notes) > 0 {
			log.Printf("WARN: 补领清扫 %s 接受: %s", label, strings.Join(notes, "；"))
		}
	}
	if codes := acceptPendingCodes(mp); len(codes) > 0 {
		n, notes := p.acceptPendingWithFallback(context.Background(), a, codes, true)
		accepted += n
		if len(notes) > 0 {
			log.Printf("WARN: 补领清扫 %s 接受(mp): %s", label, strings.Join(notes, "；"))
		}
	}

	// mp 列表是默认口径的超集，按 task_code 去重（否则同一任务领两次）。
	seen := make(map[string]bool, len(tasks)+len(mp))
	onlyMP := make(map[string]bool, len(mp))
	for _, t := range mp {
		onlyMP[t.TaskCode] = true
	}
	for _, t := range tasks {
		onlyMP[t.TaskCode] = false
	}
	for _, t := range append(append([]upstream.Task{}, tasks...), mp...) {
		if t.TaskCode == "" || seen[t.TaskCode] {
			continue
		}
		seen[t.TaskCode] = true
		if !t.Claimable || t.Locked {
			continue // 锁定 = 上游未解锁（Sequential 族每日解锁一环），领也领不到
		}
		// mp 专属码走 mp 领奖（自带 Web 域 400 降级）；只在 mp 列表出现的码同办，
		// 否则 Web 域对 mp 任务 400 会变成每轮一条 WARN。
		var cerr error
		if isMPTaskCode(t.TaskCode) || onlyMP[t.TaskCode] {
			_, _, cerr = p.cfg.Upstream.ClaimRewardMP(a, t.TaskCode)
		} else {
			_, _, cerr = p.cfg.Upstream.ClaimReward(a, t.TaskCode)
		}
		if cerr != nil {
			log.Printf("WARN: 补领 %s %s: %v", label, t.TaskCode, cerr)
			continue
		}
		claimed++
	}
	return accepted, claimed, nil
}

// acceptPendingCodes 待接受的任务码：未领、未达标、未锁定、状态还不是 accepted/completed。
func acceptPendingCodes(tasks []upstream.Task) []string {
	var codes []string
	for i := range tasks {
		t := &tasks[i]
		if !t.Claimed && !t.Locked && !taskProgressReached(t) &&
			t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	return codes
}
