// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// appVersion 面板版本，透出到 /panel/api/overview。
// 本地构建保持 dev。发布构建由 CI 用 -X 注入上海时间（2026.9.26.711），不跟上游 1.x。
var appVersion = "dev"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetReserveCredits(*cfg.Pool.ReserveCredits) // 保留积分：余额低于阈值停止接单（0 = 关闭，缺省 10）
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)
	p.SetAccountPriority(cfg.Pool.AccountPriority) // 账号优先级/占比（issue #62）：空表 = 不启用
	p.SetAccountShare(cfg.Pool.AccountShare)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
		JitterMinutes:          cfg.Schedule.JitterMinutes,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.GrowthEnabled:
		log.Printf("成长任务队列已禁用（schedule.growth_enabled=false）")
	default:
		log.Printf("成长任务队列已启用：%v 点（每日自动跑一遍任务中心待办，账号间并发 %d）",
			cfg.Schedule.GrowthHours, cfg.Schedule.GrowthConcurrency)
	}
	if cfg.Schedule.JitterMinutes > 0 {
		log.Printf("排程抖动：各任务在名义整点后 0-%d 分钟（schedule.jitter_minutes）", cfg.Schedule.JitterMinutes)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           stateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	var gw *server.Handler
	// configSaveMu 串行化面板配置保存（并发两次保存会在读-改-写文件与热应用之间互踩）。
	var configSaveMu sync.Mutex
	// pn 先声明再赋值：SaveConfig 闭包要在同一条语句里捕获它（:= 的作用域从语句结束才开始）。
	var pn *panel.Panel
	pn = panel.New(panel.Config{
		Pool:       p,
		Usage:      rec,
		RequestLog: requestLog,
		Upstream:   up,
		Scheduler:  sch,
		// 后台任务的自动触发开关与成长任务排程共用热配置（schedule.growth_enabled）。
		AutoTasksEnabled: sch.GrowthEnabled,
		AuthDir:          cfg.AuthDir,
		APIKey:           cfg.APIKey,
		RedisMode:        redisMode,
		StickyCount:      sessCount,
		Version:          appVersion,
		Live:             live,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile:  stateSibling(cfg.StateFile, "output_probes.json"),
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			configSaveMu.Lock()
			defer configSaveMu.Unlock()
			return saveConfig(raw, *cfgPath, live, p, up, sch, gw, pn, cfg)
		},
	})
	// 任务频道日志落盘（重启后任务结果可回溯）；失败只 WARN，不阻塞启动。
	if err := pn.Logs().SetTaskArchive(stateSibling(cfg.StateFile, "task-logs.json")); err != nil {
		log.Printf("WARN: task log archive: %v", err)
	}
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))
	// 成长任务队列排程的执行体在面板（队列逻辑唯一实现）：注入回调，避免 scheduler → panel 循环依赖。
	sch.SetGrowthRunner(pn.RunGrowthQueueNow)
	pn.SetGrowthConcurrency(cfg.Schedule.GrowthConcurrency)

	var auditLog *server.AuditLog
	if cfg.Admin.AuditEnabled {
		al, aerr := server.NewAuditLog(cfg.Admin.AuditFile, cfg.APIKey)
		if aerr != nil {
			log.Fatalf("admin audit: %v", aerr)
		}
		auditLog = al
		log.Printf("admin audit: %s", al.Path())
	}
	gw = server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		RequestLog:   requestLog,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled:  cfg.Global.Enabled,
		AdminEnabled:   cfg.Admin.Enabled,
		MetricsEnabled: cfg.Metrics.Enabled,
		Tasks:          sch,
		Audit:          auditLog,
		BudgetLimit:    cfg.Budget.DailyCreditLimit,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 账号后台任务（taskjobs）：状态与主 state 同目录，重启后自动恢复未完成作业。
	if err := pn.StartTaskJobs(ctx, stateSibling(cfg.StateFile, "task-jobs.json")); err != nil {
		log.Fatalf("task jobs: %v", err)
	}
	defer pn.StopTaskJobs()
	pn.ResumeTaskJobs()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	if cfg.Alerting.Enabled {
		mon := alert.New(alert.Config{
			Enabled:          true,
			WebhookURL:       cfg.Alerting.WebhookURL,
			Secret:           cfg.Alerting.Secret,
			Interval:         time.Duration(cfg.Alerting.IntervalSeconds) * time.Second,
			Timeout:          time.Duration(cfg.Alerting.TimeoutSeconds) * time.Second,
			StartupGrace:     time.Duration(cfg.Alerting.StartupGraceSeconds) * time.Second,
			MinHealthyCN:     cfg.Alerting.MinHealthyCN,
			MinHealthyGlobal: cfg.Alerting.MinHealthyGlobal,
			RecoverHealthy:   cfg.Alerting.RecoverHealthy,
			BreakerThreshold: cfg.Alerting.BreakerThreshold,
			ForTicks:         cfg.Alerting.ForTicks,
			ClearTicks:       cfg.Alerting.ClearTicks,
			SendResolve:      cfg.Alerting.SendResolve,
		}, alertSource{pool: p, h: gw})
		go mon.Run(ctx)
		// 排程轮次结果汇总的推送出口（告警 webhook 同一条管道；未启用则只落面板日志）。
		pn.SetNotifier(mon.Notify)
		log.Printf("alerting: webhook on, every %ds", cfg.Alerting.IntervalSeconds)
	}

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发或本轮重试仍可补上）。
	go warmModelRates(ctx, up, p)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定
		// 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端收到
		// 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetAccountPriority/SetAccountShare/SetCreditFloor
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
// writeConfigInPlace 原地重写配置（O_TRUNC），给两种「原子替换做不到」的部署形态复用：
// 单文件 bind mount 不能被 rename 覆盖（EBUSY）；配置目录不可写但文件可写（EACCES，
// Docker 以 PUID 运行而 /app 属主是镜像内的 app，见 issue #134）。代价是失去原子性：
// 写失败时文件可能只剩半截 —— 这两种形态下本来也没有可用 tmp 的目录。
func writeConfigInPlace(path string, out []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(out)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, gw *server.Handler, pn *panel.Panel, running ...*Config) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	// 重启清单的比较基线：优先用「运行中配置」（含 env 覆盖与启动归一化）；
	// 未传入（测试/旧调用）时退化为磁盘旧值。见 restartRequiredFields。
	previous, err := ParseConfig(oldRaw)
	if err != nil {
		return nil, fmt.Errorf("parse current config: %w", err)
	}
	baseline := previous
	if len(running) > 0 && running[0] != nil {
		baseline = running[0]
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}
	// 环境变量覆盖与启动同优先级：保存路径同样应用（否则 env 托管字段会被文件值
	// 抢班——面板显示 env 值、运行却按文件值）。移植上游 PR #104。
	applyEnv(newCfg)
	if err := newCfg.normalize(); err != nil {
		return nil, err
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		// 配置所在目录不可写：Docker 里 /app 属主是镜像内的 app(10001)，而 compose 让
		// 容器以 PUID(如 1000) 运行 —— 挂载进去的 config.json 可写、目录不行，于是原子
		// 替换连 tmp 都建不出来（issue #134：write config: open /app/config.json.tmp:
		// permission denied）。文件本身可写就退回原地重写，不再要求目录可写。
		if !errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("write config: %w", err)
		}
		if inPlaceErr := writeConfigInPlace(path, out); inPlaceErr != nil {
			return nil, fmt.Errorf("write config (目录不可写，原地重写也失败): %w", inPlaceErr)
		}
	} else if err := os.Rename(tmp, path); err != nil {
		// A single-file Docker bind mount cannot be renamed over its mount
		// target (Linux returns EBUSY / "device or resource busy"). Keep the
		// atomic path for regular files, but update the mounted file in place
		// for this specific deployment shape.
		if !errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("replace config: %w", err)
		}
		if inPlaceErr := writeConfigInPlace(path, out); inPlaceErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", inPlaceErr)
		}
		_ = os.Remove(tmp)
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	p.SetReserveCredits(*newCfg.Pool.ReserveCredits)  // 保留积分热生效（0 = 关闭，缺省 10）
	p.SetAccountPriority(newCfg.Pool.AccountPriority) // 账号优先级/占比热生效（issue #62）
	p.SetAccountShare(newCfg.Pool.AccountShare)
	sch.Reconfigure(scheduler.ScheduleParams{
		CheckinHours:   newCfg.Schedule.CheckinHours,
		TravelHours:    newCfg.Schedule.TravelHours,
		ActivityHours:  newCfg.Schedule.ActivityHours,
		KeepaliveHours: newCfg.Schedule.KeepaliveHours,
		BlackcatHours:  newCfg.Schedule.BlackcatHours,
		GrowthHours:    newCfg.Schedule.GrowthHours,

		CheckinDisabled:        !newCfg.Schedule.CheckinEnabled,
		TravelDisabled:         !newCfg.Schedule.TravelEnabled,
		ActivityDisabled:       !newCfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:      !newCfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:       !newCfg.Schedule.BlackcatEnabled,
		GrowthDisabled:         !newCfg.Schedule.GrowthEnabled,
		IncludeDisabledInTasks: newCfg.Schedule.IncludeDisabledInTasks,

		JitterMinutes: newCfg.Schedule.JitterMinutes,
	})
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	if pn != nil {
		pn.SetGrowthConcurrency(newCfg.Schedule.GrowthConcurrency) // 排程轮次并发热生效
	}
	if gw != nil {
		gw.SetBudgetLimit(newCfg.Budget.DailyCreditLimit)
	}

	return restartRequiredFields(baseline, newCfg), nil
}

// restartRequiredFields 比较运行中装配值与新配置（有运行配置时以它为准，否则退化
// 为磁盘旧值），只报告「确实变化且无法热生效」的字段——第二次保存不再重复携带上
// 一处的待重启项、改回原值即自动清除（无独立的 pending 集合）。移植上游 PR #104；
// 字段集保留本 fork 自有项（admin/metrics/alerting 整块）。
func restartRequiredFields(current, next *Config) []string {
	var out []string
	changed := func(name string, before, after any) {
		if !reflect.DeepEqual(before, after) {
			out = append(out, name)
		}
	}
	changed("listen", current.Listen, next.Listen)
	changed("auth_dir", current.AuthDir, next.AuthDir)
	changed("state_file", current.StateFile, next.StateFile)
	changed("upstream.timeout_seconds", current.Upstream.TimeoutSeconds, next.Upstream.TimeoutSeconds)
	changed("upstream.header_timeout_seconds", current.Upstream.HeaderTimeoutSeconds, next.Upstream.HeaderTimeoutSeconds)
	changed("upstream.idle_timeout_seconds", current.Upstream.IdleTimeoutSeconds, next.Upstream.IdleTimeoutSeconds)
	changed("upstream.user_agent", current.Upstream.UserAgent, next.Upstream.UserAgent)
	changed("upstream.client_version", current.Upstream.ClientVersion, next.Upstream.ClientVersion)
	changed("upstream.cli_version", current.Upstream.CliVersion, next.Upstream.CliVersion)
	changed("upstream.client_name", current.Upstream.ClientName, next.Upstream.ClientName)
	changed("upstream.device_token", current.Upstream.DeviceToken, next.Upstream.DeviceToken)
	changed("upstream.device_token_file", current.Upstream.DeviceTokenFile, next.Upstream.DeviceTokenFile)
	changed("upstream.passthrough_ip", current.Upstream.PassthroughIP, next.Upstream.PassthroughIP)
	changed("global.enabled", current.Global.Enabled, next.Global.Enabled)
	changed("global.chat_base", current.Global.ChatBase, next.Global.ChatBase)
	changed("global.billing_base", current.Global.BillingBase, next.Global.BillingBase)
	changed("prompt.mode", current.Prompt.Mode, next.Prompt.Mode)
	changed("prompt.file", current.Prompt.File, next.Prompt.File)
	changed("prompt.text", current.PromptText, next.PromptText)
	changed("upstash.url", current.Upstash.URL, next.Upstash.URL)
	changed("upstash.token", current.Upstash.Token, next.Upstash.Token)
	changed("session_sticky.enabled", current.SessionSticky.Enabled, next.SessionSticky.Enabled)
	changed("session_sticky.ttl", current.SessionTTL, next.SessionTTL)
	changed("session_sticky.gc_interval", current.SessionGCInterval, next.SessionGCInterval)
	changed("admin", current.Admin, next.Admin)
	changed("metrics", current.Metrics, next.Metrics)
	changed("alerting", current.Alerting, next.Alerting)
	changed("logging.request_archive_enabled", current.Logging.RequestArchiveEnabled, next.Logging.RequestArchiveEnabled)
	changed("logging.request_retention_days", current.Logging.RequestRetentionDays, next.Logging.RequestRetentionDays)
	changed("logging.request_archive_max_mb", current.Logging.RequestArchiveMaxMB, next.Logging.RequestArchiveMaxMB)
	changed("server.read_timeout", current.Server.ReadTimeout, next.Server.ReadTimeout)
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
