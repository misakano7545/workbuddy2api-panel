// growth.go 成长任务队列排程：每日把「任务中心」的待办自动跑一遍。
//
// 执行体不在这里——面板的队列（panel/taskcenter.go）才是唯一实现：扫描 → 按账号
// 排队 → accept → 动作 → 达标自动领奖。本文件只做排程侧入口，由 main 装配期用
// SetGrowthRunner 注入 panel 的实现（panel 依赖本包，反向 import 会成环）。
//
// 语义要点：
//   - 与面板手动「执行队列」共用同一队列状态与同一把 per-account 锁：排程跑起来时
//     手动点击会被 409/跳过，不会同一账号并发两轮动作。
//   - 幂等：任务已完成/已领奖的账号扫不出待办，跑一遍即空转（只有只读的列表查询）。
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
		log.Printf("growth 队列：已启动 %d 项待办（进度见面板任务中心）", total)
		return
	}
	log.Printf("growth 队列：%s", msg)
}
