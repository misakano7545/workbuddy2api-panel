// growth.go 成长任务排程：每日把「任务中心」的待办自动跑一遍。
//
// 执行体不在这里——面板后台任务系统（panel/taskjobs.go）才是唯一实现：调度器到点
// 把「为所有国区账号启动持久化后台作业」交给面板（scan/accept/动作/领奖都在后台
// 作业里跑，调度器不等待完整扫描）。本文件只做排程侧入口，由 main 装配期用
// SetGrowthRunner 注入 panel 的实现（panel 依赖本包，反向 import 会成环）。
//
// 语义要点：
//   - 与面板手动启动的后台任务共用同一 per-account 锁与同一份作业状态：已在执行
//     的账号复用现有作业，不会同一账号并发两轮动作。
//   - 幂等：任务已完成/已领奖的账号在后台作业里跳过动作，跑一遍即空转。
package scheduler

import (
	"log"
)

// RunGrowthNow 立即执行一轮成长任务队列（也可由 /admin/tasks/growth/run 手动补跑）。
// 未注入执行器（nil）时静默返回：测试与未装配面板的部署不必额外判断。
func (s *Scheduler) RunGrowthNow() {
	s.schedMu.Lock()
	fn := s.cfg.GrowthRunner
	s.schedMu.Unlock()
	if fn == nil {
		return
	}
	total, msg := fn()
	if total > 0 {
		log.Printf("growth 后台任务：已启动 %d 个账号（进度见面板任务中心）", total)
		return
	}
	log.Printf("growth 后台任务：%s", msg)
}
