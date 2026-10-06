// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// blackcatEligible 夜猫子候选资格：禁用与暂停选号的账号都不跑。
//
// 抽成纯函数只为一件事：RunBlackcatNow 有 23:00–08:00 窗口前置判断，窗口外的单测
// 走不到循环体，这条口径就没有可跑的检查。**取整个 Status 而不是两个布尔**——调用点
// 写不出参数反序（那是这类两布尔判据最现实的错法）。
func blackcatEligible(st pool.Status) bool { return !st.Disabled && !st.Paused }

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
func (s *Scheduler) RunBlackcatNow() {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	for _, st := range s.cfg.Pool.List() {
		// 暂停选号（paused）账号跳过：夜猫子是全任务体系里**唯一「整任务都是真实模型
		// 对话」**的（RunNightChats 逐条 ChatStream 发 glm-5.2 短对话），与「让位防风控」
		// 正面冲突。旅行 / 成长任务 / 连登管家都走上报与领奖 RPC，照常参与
		// （吸收上游 b1a2284 的口径修正）。
		if !blackcatEligible(st) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		if need <= 0 {
			continue
		}
		ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(a.UID, a.Nickname), ok, need, err)
			continue
		}
		log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(a.UID, a.Nickname), ok)
		time.Sleep(activityAccountDelay)
	}
}
