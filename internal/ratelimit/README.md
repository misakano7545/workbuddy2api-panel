# internal/ratelimit

模型限流映射：额度口径、降级链解析、策略持久化与导出包。三处消费方共用同一段代码：

- 面板「模型限流映射」页（internal/panel/modellimits.go）：读快照、改策略、导出；
- 请求路径（internal/server/handler.go）：选号前拦/改道，上游 6004 时逐级降级；
- 导出包（BuildBundle）：供上游仓库对齐同一套口径（"为提交上游做准备"）。

## 为什么自包含

本包**不 import 仓库内其它 internal 包**。用量桶、账号快照、模型倍率、池的降级表
全部经 Deps 闭包注入。两个后果都是想要的：

1. 单测不需要起池子或记录器（参见各 _test.go 里的 testDeps）；
2. 提上游时只带走这一个目录 + 两个调用点，不会把 fork 的本地增量一起拖进去。

## 文件

| 文件 | 职责 |
| --- | --- |
| ratelimit.go | 类型（Policy/ModelLimit/Preset）、预设、校验、生效口径合并（EffectiveLimit/ResolveQuota） |
| chain.go | 降级链解析：人工链 > pool.model_fallback 表 > 自动链（零倍率优先、其次最便宜） |
| store.go | 策略文件读写（tmp+rename 原子写，0600，版本化） |
| view.go | 快照（View/Row）与决策（DecisionOf/Decision）、预览、格式化 |
| tracker.go | 进程内状态：原子快照 + 30s 后台重算、Decide/NextChainTarget 给请求路径用 |
| export.go | 导出包（Bundle：JSON 给机器复算 + Markdown 给评审读） |

## 三条不能破的不变量

1. **跟随上游的行永不拦**。DecisionOf 只对 Source=manual 且 LimitState != ok 的行返回动作。
   默认口径是 follow，所以开这个功能不改变任何既有请求的行为。
2. **6004 的冷却记在本轮实际出站的模型（handler 的 curModel）名下**。上游按「账号 ×
   模型」发配额，撞墙的是刚被拒的那个模型：记成请求原模型会在多级降级里把已经放弃的
   模型反复记一遍，而真正撞墙的等价模型仍被当成健康（下一条同类请求立刻再撞一次）。
   降级目标在收到错误之前不会被冷却 —— applyErrorPolicy 只在拿到错误后调用。
3. **口径只有一处实现**。页面显示「还剩多少」与请求路径「拦不拦」必须走同一份快照。

## 页面口径

- **已用量按「域 × 账号 × 实际出站模型」归属**：降级（6004 / 本地限流改道）之后记在
  等价模型名下，见 internal/server/resolve_model.go 的 attemptModel。记账混到请求原模型
  上会让那一行虚高、显示「已用 > 池级配额」，并把兜底模型压成 0 —— 页面与请求判定
  用的是同一份数，所以归属错了连判定一起错。
- **黑名单模型整体不出现**：Deps.Hidden 读 livecfg 的 hidden 名单，目录、覆盖项、倍率表
  与降级链一并过滤（留在倍率表里会先占链名额、再被删掉，把付费兜底挤没）。
- **分类汇总别名整类不出现**：deep-model / balanced-model / fast-model / primary-model /
  default-model / auto / auto-chat 是客户端把一组模型折叠出来的入口（deep-model 在 chat
  端点直接 11102），见 aliases.go。它们不进页面、不进降级链、不占链名额 —— 与黑名单
  走同一条 dropRule，但不可解除：黑名单是运营的决定，别名是模型本身就不存在。
- **推算式额度会随上游收紧**（adaptQuota）：窗口内出现 6004 就把该域该模型的单账号额度
  压到「已用 + 10%」，只收紧不放大；手动配额不受信号改写。
- **行序按池级已用额度降序**（打开页面第一眼就是谁在烧钱），已用相同再按剩余可用
  额度升序（快见底的更靠前），前后端同一口径（view.go sortRows 与 app.js
  mrlSortRows）。百分比文本显示真实值（可 > 100%），只有进度条宽度封顶 ——
  宽度是刻度，不是统计量。

## 接到新宿主上

两个调用点，都不超过 20 行：

```go
tr := ratelimit.NewTracker(ratelimit.TrackerConfig{Path: "<数据目录>/model_limits.json", Deps: ratelimit.Deps{...}, Logf: log.Printf})
go tr.Start(ctx)                 // 后台重算；ctx 结束即退出

// 1) 选号前
if d := tr.Decide(realm, model); d.Action == ratelimit.ActionHold {
	return errTooManyRequests        // 本地预算/账号额度已尽
} else if d.Action == ratelimit.ActionDegrade {
	model = d.Target                 // 改道到链首
}

// 2) 上游 6004 时（多级链）
for to := tr.NextChainTarget(realm, model, tried); to != ""; to = tr.NextChainTarget(realm, model, tried) {
	tried[to] = true
	// 换模型重试
}
```

## 测试

```bash
go test ./internal/ratelimit/
```

不需要数据库、网络或环境变量。策略文件一律写在 t.TempDir() 里。
