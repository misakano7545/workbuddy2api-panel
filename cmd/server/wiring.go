package main

import (
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/alert"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/ratelimit"
	"github.com/misakano7545/workbuddy2api-panel/internal/server"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.WeightedAvailableUIDsForModelRealm 退化为现状
// （AvailableUIDsForModel），老调用（无前缀模型名）语义零改动。
//
// 返回列表可能对快过期账号重复同一 UID，作为虚拟实例权重；会话哈希分配无需感知
// 权重细节，已有绑定的快路径仍直接返回原账号，不做迁移。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}

type alertSource struct {
	pool *pool.Pool
	h    *server.Handler
}

func (s alertSource) Health(realm string) alert.Health {
	h := s.pool.RealmHealth(realm)
	return alert.Health{
		Total: h.Total, Healthy: h.Healthy, Cooling: h.Cooling, Disabled: h.Disabled,
		Breaker: h.Breaker, Degraded: h.Degraded, InFlight: h.InFlight,
	}
}

func (s alertSource) WAFActive() bool { return s.h.WAFActive() }

// newModelLimitsTracker 装配模型限流映射 tracker（internal/ratelimit 的唯一实例）。
//
// 为什么全部用闭包注入而不是先算好一份快照：这里的每一个依赖（账号池、用量、倍率、
// 降级表）都是**活数据**，tracker 每次 Refresh 都要重新读。先快照再喂进去，等于把
// 「刷新间隔」变成「装配时刻」，页面上的额度会一直停在启动那一刻。
//
// 策略文件与 state.json 同目录（<数据目录>/model_limits.json）：跟 state 一起搬移，
// 不再多一个配置项；迁移数据目录时策略不会留在旧路径上。
//
// 面板与网关注入的是**同一个** tracker —— 面板改完策略，请求路径立即按新策略判定。
func newModelLimitsTracker(p *pool.Pool, up *upstream.Client, rec *usage.Recorder, live *livecfg.Holder, stateFile string) *ratelimit.Tracker {
	path := stateSibling(stateFile, "model_limits.json")
	return ratelimit.NewTracker(ratelimit.TrackerConfig{
		Path: path,
		Deps: ratelimit.Deps{
			// 窗口内 (域, 账号, 模型) 的消耗：配额是按账号发的，必须保留账号这一维。
			Usage: func(d time.Duration) []ratelimit.UsageRow {
				var w usage.Window
				if d > 0 {
					w.Hours = int(math.Ceil(d.Hours()))
					if w.Hours < 1 {
						w.Hours = 1
					}
				}
				rows := rec.ModelWindow(w)
				out := make([]ratelimit.UsageRow, 0, len(rows))
				for _, r := range rows {
					out = append(out, ratelimit.UsageRow{
						Realm: r.Realm, UID: r.UID, Model: r.Model,
						Tokens: r.Tokens, Requests: r.Requests, Errors: r.Errors, Credits: r.Credits,
						FirstAt: r.FirstAt,
					})
				}
				return out
			},
			// 账号状态：额度分摊与「可路由账号数」都按它算（禁用/暂停/冷却的号不摊）。
			Accounts: func() []ratelimit.Account {
				list := p.List()
				out := make([]ratelimit.Account, 0, len(list))
				for _, s := range list {
					a := ratelimit.Account{
						UID: s.UID, Nickname: s.Nickname, Realm: s.Realm,
						Credits:  float64(s.Credits),
						Cooling:  s.Cooling,
						Disabled: s.Disabled,
						Paused:   s.Paused,
					}
					if n := len(s.RateLimitedModels); n > 0 {
						a.RateLimited = make([]ratelimit.RateLimit, 0, n)
						for _, rl := range s.RateLimitedModels {
							a.RateLimited = append(a.RateLimited, ratelimit.RateLimit{
								Model: rl.Model, Kind: rl.Kind, Reason: rl.Reason,
								Until: rl.Until, ResetAt: rl.ResetAt,
							})
						}
					}
					out = append(out, a)
				}
				return out
			},
			// 域集合：静态两域 + 池里实际出现过的域（企业号/未来新域自动纳入，
			// 否则页面会少一整列「这个域还有多少额度」）。
			Realms: func() []string {
				seen := map[string]bool{"cn": true, "global": true}
				for _, s := range p.List() {
					if s.Realm != "" {
						seen[s.Realm] = true
					}
				}
				out := make([]string, 0, len(seen))
				for r := range seen {
					out = append(out, r)
				}
				sort.Strings(out)
				return out
			},
			// 倍率表：与请求路径 ModelRate 同一来源（upstream 的实时快照）。
			Rates: func(realm string) map[string]string {
				if up == nil {
					return nil
				}
				return up.ModelRatesSnapshot(realm)
			},
			// 可用模型：取该域倍率表的键（上游实际报过价的模型 = 该域真的能跑）。
			Models: func(realm string) []string {
				if up == nil {
					return nil
				}
				snap := up.ModelRatesSnapshot(realm)
				if len(snap) == 0 {
					return nil
				}
				out := make([]string, 0, len(snap))
				for m := range snap {
					out = append(out, m)
				}
				sort.Strings(out)
				return out
			},
			// 老降级表：view 里与策略链并排显示，方便核对两条来源是否打架。
			Fallback: func() map[string]string {
				if live == nil {
					return nil
				}
				return live.Load().ModelFallback
			},
			// 模型黑名单：与网关 hiddenModel() 读同一份 livecfg.HiddenModels，
			// 拉黑的模型不出现在限流页、不进降级链、不参与请求路径判定。
			Hidden: func(realm string) map[string]bool {
				if live == nil {
					return nil
				}
				liveSnap := live.Load()
				if len(liveSnap.HiddenModels) == 0 {
					return nil
				}
				out := make(map[string]bool)
				for key := range liveSnap.HiddenModels {
					r, bare, ok := strings.Cut(key, ":")
					if !ok || r != realm {
						continue
					}
					out[bare] = true
				}
				if len(out) == 0 {
					return nil
				}
				return out
			},
			Now: time.Now,
		},
		Logf: log.Printf,
	})
}
