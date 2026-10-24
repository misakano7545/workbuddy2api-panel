// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/httpauth"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/prompt"
	"github.com/misakano7545/workbuddy2api-panel/internal/reqlog"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// ModelFallback 模型降级路由表（pool.model_fallback）：key = 请求模型（裸名或带域
	// 前缀均可），value = 6004 限流时改用的等价模型。空/nil = 不降级（老行为）。
	// 只对 6004 模型级限流生效；降级不写持久状态，冷却到期自动回原模型。
	ModelFallback func() map[string]string

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// ModelsHidden 模型黑名单原始条目（config models.hidden；Live 为 nil 的
	// 裸用/测试路径经 loadLive() 归一化后使用）。
	ModelsHidden []string

	// ModelsAliases 模型映射原始条目（config models.aliases，"客户端名=真实名"；
	// issue #30；Live 为 nil 的裸用/测试路径经 loadLive() 归一化后使用）。
	ModelsAliases []string

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	Usage *usage.Recorder

	// AdminEnabled 为 true 才注册 POST /admin/tasks/{name}/run。缺省关闭。
	AdminEnabled bool
	// MetricsEnabled 为 true 才注册 GET /metrics。缺省关闭。
	MetricsEnabled bool
	// Tasks 手动补跑。nil 时开了 admin 也回 503。
	Tasks TaskRunner
	// Audit 非 nil 时 /admin 操作追加 JSONL。
	Audit *AuditLog
	// BudgetLimit <=0 不拦。按 CST 日累计观测到的 credit。
	BudgetLimit float64
	// BurstCreditLimit <=0 不拦：5 分钟窗口内累计 credit 达此值即拒（issue #229）。
	BurstCreditLimit float64
	// CacheLowRate <=0 不拦：连续 5 轮命中率低于此百分比即冷却 60s（issue #229）。
	CacheLowRate float64
	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder

	// RecordClientInfo 是否在请求日志里记录调用来源（客户端 IP / User-Agent）。
	// 来自 logging.request_client_info（缺省 true）；关闭时 reqlog 事件的来源字段
	// 保持为空，归档与面板都不出现来源信息。
	RecordClientInfo bool
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:           h.cfg.APIKey,
		SoftCooldown:     h.cfg.SoftCooldown,
		RecordClientInfo: h.cfg.RecordClientInfo,
		HiddenModels:     livecfg.NormalizeHiddenModels(h.cfg.ModelsHidden),
		ModelAliases:     livecfg.NormalizeModelAliases(h.cfg.ModelsAliases),
	}
}

// modelAlias 查 aliases 映射（issue #30）：客户端名（剥 realm 前缀后的裸名，
// 大小写不敏感）命中映射 → 返回真实模型名（原样，可再带 realm 前缀跨域指向）；
// 未命中 → ("", false)。查找键与 livecfg.NormalizeModelAliases 的归一化口径配对：
// realm+":"+小写裸名。
func (h *Handler) modelAlias(realm, bare string) (string, bool) {
	if bare == "" {
		return "", false
	}
	target, ok := h.loadLive().ModelAliases[strings.ToLower(realm)+":"+strings.ToLower(bare)]
	return target, ok
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// gatewayUnauthorizedReason 网关 401（无效 token）达阈禁用时写入 state.json 的 reason。
// 与 12153 分开写：面板的「禁用原因」是运维第一手线索——看到 12153 会去查 refresh
// 链路，而网关 401 是 token 被边缘直接判无效，只能重新登录。
const gatewayUnauthorizedReason = "401 invalid token (re-login required)"

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP       wafIPGate
	taskMu      sync.Mutex
	taskRunning map[string]bool
	budget      *dailyBudget
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	h := &Handler{
		cfg:         cfg,
		mux:         http.NewServeMux(),
		taskRunning: map[string]bool{},
		budget:      newDailyBudget(cfg.BudgetLimit),
	}
	h.budget.setGuards(cfg.BurstCreditLimit, cfg.CacheLowRate)
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	// Codex 客户端在 supports_websockets/prefer_websockets 时先探 GET /v1/responses 升级 WS。
	// 本网关不做连接内 previous_response_id 复用，回 426 让它立刻粘性回落 HTTPS。
	h.mux.HandleFunc("GET /v1/responses", h.withAuth(h.responsesWebsocketUnsupported))
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(func(w http.ResponseWriter, r *http.Request) { h.images(w, r, false) }))
	h.mux.HandleFunc("POST /v1/images/edits", h.withAuth(func(w http.ResponseWriter, r *http.Request) { h.images(w, r, true) }))
	h.mux.HandleFunc("POST /v1/messages", h.withAnthropicAuth(h.messages))
	h.mux.HandleFunc("POST /messages", h.withAnthropicAuth(h.messages))
	// 官方 agenttool 联网检索/抓页的网关透传（非对话端点，与 images 同套取号/失败处置）。
	h.mux.HandleFunc("POST /v1/search", h.withAuth(h.search))
	// DeepSeek Fetch API 形状（DSH 的 web-deepseek-api 把 /search 与 /fetch 指向同一 base）。
	h.mux.HandleFunc("POST /v1/fetch", h.withAuth(h.fetch))
	// POST /v2/chat/busy：官方客户端的「模型忙」预检（国区上游 200、国际版上游 404，
	// 见 busy.go 文件头）。本地按池台账算，不打上游。
	h.mux.HandleFunc("POST /v2/chat/busy", h.withAuth(h.chatBusy))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /v1/capacity", h.withAuth(h.capacity))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	if cfg.AdminEnabled {
		h.mux.HandleFunc("POST /admin/tasks/{name}/run",
			h.withAuth(h.audit("task.run", auditPathValue("name"), h.adminTaskRun)))
	}
	if cfg.MetricsEnabled {
		h.mux.HandleFunc("GET /metrics", h.withAuth(h.promMetrics))
	}
	return h
}

// isChatEntry 四条入站协议路径：/v1/responses 与 /v1/messages（含 /messages 别名）
// 经 r.Clone 转进 chatCompletions 内核，与直连的 /v1/chat/completions 共用同一套
// 行日志/归档/来源采集/X-Request-Id——四者一视同仁，别再按单路径放行（Codex 走
// /v1/responses，曾因此整类请求无来源、不入归档）。
func isChatEntry(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages", "/messages",
		"/v1/images/generations", "/v1/images/edits":
		return true
	}
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && r.Method == http.MethodPost && isChatEntry(r.URL.Path) {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: r.URL.Path}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	budgetUsed, budgetLimit, budgetRejected, budgetLastReject := h.budget.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// model_locks 当前有未过期模型级限流的 (域, 模型) 全清单（移植上游 PR #120）：
		// accounts 回答「哪些号不能用」，本键回答「哪些模型不能用、锁了几个号、还要锁多久」。
		// 与 ModelBlocked（请求失败时的单模型判定）互补；无锁时为 null。只增键，零回归。
		"model_locks": h.cfg.Pool.ModelLockView(),
		// credit_floor 生效的积分保底值（0 = 关闭）。与 accounts[].credits +
		// model_costs 对照即可判定「某号为何对某模型不出票」。零值也显式写出
		// （运维口径：缺失会让人误以为没记录）。
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
		"daily_budget": map[string]any{
			"used":        budgetUsed,
			"limit":       budgetLimit,
			"rejected":    budgetRejected,
			"last_reject": budgetLastReject,
			"day":         cstDay(time.Now()),
		},
	})
}

func (h *Handler) capacity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if strings.TrimSpace(h.loadLive().APIKey) == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "API key is required")
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	realm, bareModel := resolveModel(model)
	if target, ok := h.modelAlias(realm, bareModel); ok && target != "" {
		realm, bareModel = resolveModel(target)
	}
	modelAvailable, poolAvailable := h.cfg.Pool.CapacityForModel(bareModel, realm)
	if h.hiddenModel(realm, bareModel) {
		modelAvailable = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":                    model,
		"model_available_accounts": modelAvailable,
		"available_accounts":       poolAvailable,
	})
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	// dynamicModelsTTL 模型目录缓存时长。曾是 1h；缩到 10min 对齐「面板实时、
	// API 缓存」的漂移痛点（PR #38 报告）：目录新增模型时面板立即可见，公开
	// /v1/models 最多滞后一个 TTL。再短就不值得——每次失效都是 2 次上游探测。
	dynamicModelsTTL        = 10 * time.Minute
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 10min），失败/无号返回空列表（无静态兜底——
// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	// 付费层级（issue #235）：官方 badge 原文，如 global「Pro Only」/ CN「订阅优先」。
	// 只标注不过滤——层级是账号级属性，列表是全池共享的。
	if mi.Tier != "" {
		entry["tier"] = mi.Tier
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
		if mi.CanDisableThinking {
			entry["can_disable_thinking"] = true
		}
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// hiddenModel 报告 (realm, 裸模型名) 是否被 config models.hidden 隐藏。
//
// 黑名单语义是「这个模型对本网关不存在」：列表不列、点名调用直接拒绝。四个生效点
// （cn 对话列表 / global 对话列表 / 出图单列 / chat 与出图入口）共用本判据，避免
// 各写一份 map 查表而漏掉某一处——漏一处就等于黑名单可被绕过。
func (h *Handler) hiddenModel(realm, bare string) bool {
	if bare == "" {
		return false
	}
	_, hide := h.loadLive().HiddenModels[realm+":"+bare]
	return hide
}

// modelList 模型列表：CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel
// 对称）；global.enabled=true 时追加 global: 前缀的国际版名单。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	for _, mi := range h.fetchDynamicModels() {
		if h.hiddenModel("cn", mi.ID) {
			continue
		}
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.model_catalog）：
		// 上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态种子表 →
		// model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，拉到后
		// 写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （客户端可发现档位，不再盲传）。无档位 → 省略字段。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 纯动态探测结果（fetchGlobalModels，失败/无号 → 空）。
	if h.cfg.GlobalEnabled {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			if h.hiddenModel("global", id) {
				continue
			}
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（与 CN 动态分支同口径）。
			var remoteCtx, remoteOut int64
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				remoteCtx, remoteOut = mi.ContextWindow, mi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, h.cfg.Upstream.HTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, h.cfg.Upstream.HTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	// 出图模型（目录里被 tag 剔掉的 text-to-image / image-to-image）：对话列表照旧不含它们，
	// 这里以 image_generation=true 单列——客户端据此发现可用的出图模型名，不会误发对话。
	out = append(out, h.generationModelEntries()...)
	return out
}

// generationModelEntries /v1/models 里的出图模型条目（来自最近一次目录解析的旁路留存，
// 零额外上游请求；尚未探测过该域时为空）。字段只给确定值：不编 context/output。
func (h *Handler) generationModelEntries() []map[string]any {
	if h.cfg.Upstream == nil {
		return nil
	}
	realms := []string{"cn"}
	if h.cfg.GlobalEnabled {
		realms = append(realms, "global")
	}
	var out []map[string]any
	for _, realm := range realms {
		for _, mi := range h.cfg.Upstream.GenerationModels(realm) {
			// 只列图片：视频没有网关端点（CN 侧无查询路由、视频走连接器），列出来只会
			// 引诱客户端去调不存在的路由。视频模型照旧不进对话名单（nonChatModel）。
			if mi.GenerationKind != "image" {
				continue
			}
			// 黑名单同样管出图单列：列表里留着它等于告诉客户端「这个能用」。
			if h.hiddenModel(realm, mi.ID) {
				continue
			}
			entry := map[string]any{
				"id":               realm + ":" + mi.ID,
				"object":           "model",
				"created":          1753600000,
				"owned_by":         "workbuddy",
				"image_generation": true,
			}
			out = append(out, applyModelInfoFields(entry, mi))
		}
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 缓存/失败回落封在 upstream.FetchGlobalModels（内部 1h + 5min 负缓存）。
// 本方法只负责"何时探测"：池中无 global 账号 → 空名单 + nil 账号（零上游调用）。
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// fetchDynamicModels 从第一个可用 CN 账号拉模型列表（含 contextWindow/maxTokens），
// 缓存 10min。
// 选号与 /panel/api/models 完全同口径（AvailableUIDsForRealm("cn") 首个 + AuthByUID），
// 而非 Pool.Pick()：Pick 无 realm 过滤，混合池里可能选中 global 号去打 CN 端点，
// 表现为偶发失败/面板与 /v1/models 两套目录（PR #38 报告并给出的选号修复）。
// 缓存 + 5min 负缓存按既有语义**保留**（#38 原案整体删除缓存被拒）：公开端点逐请求
// 实时拉取 = 每次 2 个上游探测，客户端周期性刷新模型列表会持续打上游；上游故障时
// 无冷却窗口，客户端重试即放大请求量——负缓存正是为此设计（见 handler_test 吸收
// 上游 9832283 的注释）；且 cachedModelsSnapshot（gateway_hint 判定）依赖缓存写入。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	uids := h.cfg.Pool.AvailableUIDsForRealm("cn")
	if len(uids) == 0 {
		return nil
	}
	acct := h.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError：NoteError 喂的是 chat
		// 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道健康的账号；
		// models 拉取失败 ≠ 账号 chat 不可用。
		// 只在真正打上游失败时记一行（负缓存期内前面已直接 return，不会刷屏）：
		// 此前这里静默，CN 域不可达时 /v1/models 只剩 global 模型却毫无线索。
		if err == nil {
			err = fmt.Errorf("models api returned empty list")
		}
		log.Printf("WARN: [server] /v1/models: CN 模型目录拉取失败（%s 内不再重试，cn: 模型暂不列出）: %v",
			modelsFetchFailCooldown, err)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（hint 判定用：错误路径加一次 FetchModels 网络调用既拖慢
// 错误响应、又污染上游调用语义）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if ok, why := h.budget.admit(); !ok {
		used, limit, _, _ := h.budget.snapshot()
		code, msg := "daily_budget_exceeded", fmt.Sprintf(
			"daily credit budget exhausted (used %.2f of %.2f, resets at 00:00 CST)", used, limit)
		if why != "" {
			// 快速闸的拒因自带可读说明，别和「当日用光」混成一句话
			code, msg = "credit_guard_tripped", why
		}
		writeOpenAIError(w, http.StatusTooManyRequests, code, msg)
		return
	}
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 请求体无大小上限（max_body_mb 已移除，对齐上游）：完整读入，超限类问题交由
	// 上游自然返回错误（其响应经既有错误分类链路透出，信息量更大）。#41 的截断
	// 防御语义保留在读错误路径——移除预拦截后，截断只可能来自客户端自己断流，
	// 读 body 出错就地 400，不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 冤枉罚号。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本（见
	// upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的 tools」与
	// 「会话历史里出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失，
	// 只认前者会让修复层在最需要它的场景下失效。
	// 在提示词/模型名改写之前取（那些改写不动 tools / messages[].tool_calls 子树）。
	declaredTools := toolNamesIfPresent(body)

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel := resolveModel(peek.Model)

	// models.aliases 映射（issue #30）：客户端名命中映射 → 出站/选号/effort 全部
	// 改用目标名，realm 也按目标重解析（别名值可带前缀跨域指向）。
	// 放在 hidden 检查**之前**：被映射到的目标仍受黑名单约束，别名不能绕过隐藏。
	// 出站 body 同步重写顶层 model（prepareBody 的 normalizeModelName 只剥前缀，
	// 不认识别名——名字必须在这里就换掉）。未命中 → body 一个字节不动（现状零回归）。
	// modelEcho 非空 = 本请求走了映射，响应侧（SSE 帧 / 聚合体）把 model 覆写回
	// 客户端原发名——严格校验「响应 model == 请求 model」的客户端不会当场报错。
	// routeModel = 选号/粘性/账本使用的模型名（映射后为真实名，可带 realm 前缀）。
	modelEcho := ""
	routeModel := peek.Model
	if aliasTarget, aliased := h.modelAlias(realm, bareModel); aliased && aliasTarget != "" {
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			if cur, isStr := obj["model"].(string); isStr && cur == peek.Model {
				obj["model"] = aliasTarget
				if nb, merr := json.Marshal(obj); merr == nil {
					body = nb
					realm, bareModel = resolveModel(aliasTarget)
					modelEcho = peek.Model
					routeModel = aliasTarget
					log.Printf("INFO: [server] model alias applied: %s -> %s", peek.Model, aliasTarget)
				}
			}
		}
	}
	// 模型降级（pool.model_fallback）：curModel 是**本轮实际使用**的模型，初值 = 客户端
	// 请求的 bareModel；某账号在本轮模型上被 6004 模型级限流、且配置了等价模型时，
	// 本账号换用等价模型重试（curModel 改指向等价模型），随后重置回 bareModel。
	//
	// ⚠️ 为什么必须与 bareModel 分开：降级后二者指向**不同模型**，而它们要喂给不同用途 ——
	//   - **实际用途**（选号 / 出站 body 重写 / 成本记账 / 提示语）→ curModel
	//   - **冷却记账**（applyErrorPolicy 的 6004 分支）→ bareModel（**限流的是原模型**！
	//     若误记成等价模型，会把本来好好的等价模型也冷却掉，降级就白做了）
	curModel := bareModel
	// fallbackTried 记录本请求**已试过的降级目标**，防 A→B→A 循环。
	// 与 tried（按账号去重）正交：降级是「同账号换模型」，故需独立追踪。
	fallbackTried := map[string]bool{}
	// fallbackTo 返回 curModel 的等价模型（未配置 / 已试过 / 自指 → 空）。
	fallbackTo := func() string {
		if h.cfg.ModelFallback == nil {
			return ""
		}
		m := h.cfg.ModelFallback()
		if m == nil {
			return ""
		}
		to := m[curModel]
		if to == "" || to == curModel || fallbackTried[to] {
			return ""
		}
		return to
	}
	// applyFallback 把本轮模型切到等价模型，并重写出站 body 的顶层 model。
	// 只改 curModel —— bareModel 保持原模型，供冷却记账使用。
	applyFallback := func(to string) bool {
		if to == "" {
			return false
		}
		fallbackTried[to] = true
		log.Printf("model fallback: %s -> %s (6004 model rate limit, uid pending)", curModel, to)
		curModel = to
		// 出站 body 重写：上游必须收到等价模型名。与上方 models.aliases 同一手法
		// （解析 → 改顶层 model → 重序列化），保持单一路径、避免两套改写逻辑漂移。
		// 解析失败时不动 body —— 那说明请求体本就不是合法 JSON，后续会按原样失败。
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			obj["model"] = to
			if nb, merr := json.Marshal(obj); merr == nil {
				body = nb
			}
		}
		return true
	}
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	// routeModel：映射请求显示真实路由名（别名 INFO 行已记录 客户端名 -> 真实名，
	// 两行合看即全链路；流水行与账本/上游口径一致，运维排查不用做心算翻译）。
	st := newChatStat(time.Now(), routeModel, peek.Stream, h.budget)
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		// 来源在 ServeHTTP 入口采集（此时才知道开关与请求头），此处转交给统计对象，
		// 让 stdout 流水行与归档事件共用同一份来源值，两处不会漂移。
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	// models.hidden 黑名单：点名调用直接拒绝，不打上游、不占选号。
	// 与 /v1/models 的列表过滤对称（只藏列表不够，知道名字仍可硬调）。
	if h.hiddenModel(realm, bareModel) {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "model_unavailable",
			"model is hidden by gateway configuration, try another model", "")
		st.status = http.StatusBadRequest
		st.outcome = reqlog.OutcomeHTTPError
		return
	}

	tried := map[string]bool{}
	var lastErr error
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus
	// busyWaited 本次请求是否已经为「模型繁忙」等过一轮（issue #46，见 acct==nil 分支）。
	busyWaited := false

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// ExtractKey 与粘性开关解耦（issue #35 侧）：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级——提取本身与粘性无关。
	sessKey := session.ExtractKey(body)
	// 缓存槽会话段：sessKey 优先；sessKey 为空（典型是只发 user_id 的客户端，见
	// session.ExtractKey 的 anti-monopoly 闸）时用内容派生键——否则这些客户端的
	// prompt_cache_key 退化成每账号一个常量槽，同账号上多段对话互相覆盖前缀缓存
	// （issue #5：glm-5.3 大上下文周期性命中率掉到 0.2%）。
	// 只供缓存槽使用：账号绑定仍只认 sessKey，不复活 user 维度粘性。
	cacheKeyID := sessKey
	if cacheKeyID == "" {
		cacheKeyID = session.DeriveCacheKey(body)
	}
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// 按模型解析：绑定号在**当前模型**被 6004 限额时视为不可用 → 重新分配，
		// 而不是钉在限额号上反复失败（"限额后换不动号"的正解）。
		// 用 routeModel 而非 peek.Model（issue #30）：映射后必须按真实模型的
		// 可用性口径判绑定，别名名不在任何账本里。
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, routeModel); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（对齐官方桌面 CLI
	// 的 X-Conversation-Request-ID 轮级语义——TraceStartHook 每次 USER_PROMPT_SUBMIT
	// 清空重生成），故不再限 sessKey=="" 才计算；sessKey 由下方派生处以复合键方式
	// 入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite 之前取——改写会动 messages 内容，之后取会让键漂移。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := imagePartIfPresent(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// ttfb 首 token 等待（仅流式有观测；非流式传 0 = 无观测，速率不扣减）。
	// 显式入参而不是读 st.ttfb：后者在流式分支里是**调用之后**才赋值的，
	// 靠顺序传递会让将来重排代码时静默把速率算回旧的错口径（吸收上游 8b18ab8）。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		// 请求指标（PR #71）：每次尝试累加 retry 次数与 token 口径。
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = bareModel
		if delta.Model == "" {
			delta.Model = peek.Model
		}
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs

		// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算
		// 无关，所以留在调用点；速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
		if delta.HasCompletionTokens {
			if tps, ok := tokensPerSecond(delta.CompletionTokens,
				time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
				delta.HasTokensPerSecond = true
				delta.TokensPerSecond = tps
			}
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				Credit:           credit,
				HasCredit:        hasCredit,
				ModelRate:        modelRate,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
				CacheHitTokens:   delta.CacheHitTokens,
				CacheMissTokens:  delta.CacheMissTokens,
				HasCache:         delta.HasCacheTokens,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}
	// 渠道校验兜底：上游要求 messages[0] 必须是 system（首条 user 直接 400
	// 11-128「first message is not system prompt」，首条 developer 报
	// unapproved channel）——2026-10-20 真机实测三条对照。custom/append 天然
	// 以 system 开头，passthrough 透传客户端消息时可能首条就是 user，故这里统一
	// 兜一次。只补首条缺失这一种情况，其余消息逐字不动。
	body = prompt.EnsureSystemFirst(body, h.cfg.PromptText)

	// outbound model 名由 upstream.prepareBody 在同一次 JSON 解析里剥成裸名
	// （realm 前缀是网关侧路由协议，上游不认前缀）。这里不再提前做整段
	// parse+marshal，body 保留客户端原始模型名供日志/续写帧使用。

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先，否则按
	//     粘性 key 进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	// conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；
	// CacheKeyID：网关派生的粘性会话键，仅用于 prompt_cache_key 的会话段（无
	// conversation_id 的客户端也能各占一个前缀缓存槽，不再全账号共用一个）。
	chatMeta := upstream.ChatMeta{
		ConversationID: session.ResolveConversationID(body),
		CacheKeyID:     cacheKeyID,
	}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞（#170 统一轮级）。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底（无 user 消息/无可签名内容）：会话级聚合，好于请求级随机。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, curModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑，
				// 本次回落普通轮换。
				unbindSticky()
				acct = nil
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			acct = h.cfg.Pool.PickExcludingForRealm(tried, curModel, realm)
		}
		if acct == nil {
			// ⭐ 模型降级兜底（20261021 补的真实缺口）：全池该模型都处于 6004 冷却时，
			// PickExcludingForRealm 直接返回 nil —— 若只在上游返回 6004 后降级，这里会
			// **先报 503、根本走不到降级**，恰好在最需要降级的场景（全池被限流）失效。
			// 故在「选不到号」时也尝试换模型：锁定等值模型后重新进入下一轮选号。
			if to := fallbackTo(); to != "" {
				log.Printf("model fallback: %s -> %s (no account for model, 6004 blocked)", curModel, to)
				if applyFallback(to) {
					continue
				}
			}
			// issue #46：全池该模型只是被 14003「模型繁忙」的短冷却挡住时，先等它的
			// 最早解封（上限 busyWaitCap）再试，而不是把几秒的抖动直接变成客户端
			// 可见的断流。实测上游繁忙是间歇的（同一分钟内既有 14003 也有成功），
			// 而三个号的冷却叠在一起会把间歇放大成整段黑障。
			// 只等一次、不占在途名额；超过上限（冷却已被连续命中拉长）仍按 503 报。
			if !busyWaited {
				if blk := h.cfg.Pool.ModelBlockedForRealm(curModel, realm); blk.Blocked {
					if d := busyWaitFor(blk, time.Now(), busyWaitCap); d > 0 {
						w := waitPast(d)
						log.Printf("busy-wait: model=%s realm=%q %d account(s) busy, waiting %s then retry once (issue #46)", bareModel, realm, blk.Count, w.Round(time.Millisecond))
						if !sleepCtx(r.Context(), w) {
							break // 客户端已断连：不必再等
						}
						busyWaited = true
						modelBlock = blk
						i-- // 等待不算一次换号：槽位留给真正的尝试
						continue
					}
				}
			}
			st.status = http.StatusServiceUnavailable
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/11158/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			// 在这里取一次快照，供下方错误构造区分；按 realm 统计，否则另一域的
			// 健康号会把「本域全被模型级冷却挡住」掩盖成还有号可服务。
			modelBlock = h.cfg.Pool.ModelBlockedForRealm(curModel, realm)
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避）
				}
				continue
			}
			if err := acct.SaveAtomicRetry(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		var hopTiming upstream.ChatTiming
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta, &hopTiming)
		st.prep, st.do = hopTiming.Prep, hopTiming.Do
		st.effort = hopTiming.Effort
		st.effortReq = hopTiming.RequestedEffort
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析）。传输层错误（非 *Error）走抖动换号分支；防御分支
		// （terr 为 nil 但 status>=400，如 ErrNone 兜底）回落本地 Classify，双保险。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			// 客户端已断连（人走了）例外：传输错误是断连的**结果**，不是这个号的
			// 错——喂连败会把每次断连都记到当时被选中的号上，几次之后整池逐个被
			// 降权、且边降边被后续重试刷新，表现为「账号永远解不了封」；换号也只是
			// 白打上游。真传输抖动（ctx 未取消）照旧计罚。
			if r.Context().Err() != nil {
				st.outcome = reqlog.OutcomeInterrupted
				fail(acct.UID)
				break
			}
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避）
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// ⭐ 模型降级（pool.model_fallback）：**仅 6004 模型级限流**触发。
			//
			// 语义：等价模型（如 deepseek-v4.1-flash → deepseek-v4-flash）是同一底层
			// 能力，只是计价不同；贵/便宜的那个被限流时改用另一个继续服务，而不是让
			// 客户端等冷却结束。**同账号**换模型（不换号）——换模型比换号代价小。
			//
			// ⚠️ 为什么只认 6004：11102（该后端无此模型）换模型也救不了（等价模型可能
			// 同样没有）；11101/普通 429/5xx 是账号级或瞬时问题，换模型无意义。
			//
			// ⚠️ 冷却必须记在 **bareModel**（原模型）名下——见函数内 applyErrorPolicy
			// 调用处。若误记成等价模型，会把本来好好的等价模型也冷却掉，降级白做。
			if kind == upstream.ErrSoftRate && upstream.IsModelRateLimit(string(respBody)) {
				if to := fallbackTo(); to != "" {
					// 先按原模型罚账号/记冷却（限流的是原模型），再切等价模型重试。
					h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
					savedBare := bareModel
					if applyFallback(to) {
						// 同账号重试：把该账号从 tried 移除（降级是「换模型」不是「换号」，
						// 同一账号仍应有机会），并让 sticky 保持（本请求内不重绑）。
						delete(tried, acct.UID)
						bareModel = savedBare // 冷却记账口径不变，仅 curModel 改了
						releaseHeld()
						log.Printf("model fallback: %s -> %s (6004 model rate limit, uid=%s)", savedBare, to, acct.UID)
						continue
					}
				}
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样），
				// 不再改写成网关固定文案——客户端必须看到真实错误才能排查。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作，fail 只释放租约。
			// message 装上游 body 原文（含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 6021「queue.full.title」渠道级容量满载：不罚号不轮转，立即透传上游原文
			//（503 + upstream_busy，含 requestId）。上游集群排队打满时换任何账号都撞
			// 同一堵墙——按限流处置会逐号软冷却、与全冷却兜底互相喂成整池停摆
			//（2026-10-15 实案：18 次全落 soft_rate），且换号把无效请求放大
			// MaxRotate 倍。客户端按 SDK 退避重试即可；Retry-After 是本地兜底
			//（上游未给重置口径时按 5s 提示，PR #180 同款）。
			if kind == upstream.ErrUpstreamBusy {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "upstream capacity is full (queue.full); retry after a short wait"
				}
				retryAfter := "5"
				if uerr != nil && uerr.RetryAfter > 0 && int64(uerr.RetryAfter.Seconds()) > 0 {
					retryAfter = strconv.FormatInt(int64(uerr.RetryAfter.Seconds()), 10)
				}
				w.Header().Set("Retry-After", retryAfter)
				writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "upstream_busy", msg,
					h.hintOf(upstream.ErrUpstreamBusy, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11148「tool calls and tool results do not match」：请求内容级配对断裂。
			// 上游明示「直接重试无效，请新建任务」——同一 body 换任何账号都撞同一处，
			// 轮转只会把整池健康号逐个白打一遍（此前还按 ErrClient 喂连败降权），
			// 客户端等完全池后拿到 503「账号不可用」，与账号健康毫无关系。
			// fail-fast 400 透传上游原文 + hint 指向「开新会话」，不罚号不轮转。
			// shortcut: 未做「剥离工具历史后自愈重试一次」，Codex 坏会话仍需用户新建会话；
			// 补自愈时在 fail() 前重写 body 再走一次同账号尝试即可。
			if kind == upstream.ErrToolMismatch {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "upstream rejected the request: tool calls and tool results do not match"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "tool_call_mismatch", msg,
					h.hintOf(upstream.ErrToolMismatch, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 请求级参数拒绝（11101 请求体解析失败 / 11133 模型供应商拒绝参数）：与
			// 11115 / 图片无效同一哲学——同一 body 换任何账号都是同样的拒绝，轮转只会
			// 放大无效请求（每号一次上游调用 + rotateBackoff 占用在途名额）。更要紧的
			// 是：继续轮转后末端会落到「其余保持 503」，把确定失败的请求伪装成
			// "账号不可用、稍后再试"，客户端于是对必然失败的请求无限重试。立即透传
			// 上游原文回 400。（11101 吸收上游 fd5c5a6；11133 见 payload.go 实测
			// 「全号轮转同样被拒」）
			if kind == upstream.ErrBadParams || kind == upstream.ErrModelParamInvalid {
				code, fallback := "bad_params", "chat request body was rejected by upstream"
				if kind == upstream.ErrModelParamInvalid {
					code, fallback = "model_param_invalid", "request parameters were rejected by the upstream model provider"
				}
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = fallback
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, code, msg,
					h.hintOf(kind, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：
			// 该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，IP 被拦而非账号）
			// 则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍打同一出口 IP，
			// 加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避）
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 成功才带头。uid 是十六进制，昵称可能非 ASCII，不入头。
		w.Header().Set("X-Wb-Account", acct.UID)
		// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
		// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
		h.cfg.Pool.BlockModelClear(acct.UID, curModel)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			// 截断自动续写（见 continue.go）：上游 finish_reason=length 且为纯文本/推理
			// 段时，同账号同模型补发「已输出内容+续写指令」，多段拼成一条客户端可见流。
			// 显式限额 / 工具参数截断 / 续写请求失败 → 自动降级为今天的截断终态。
			cont := newContinueReader(r.Context(), h.cfg.Upstream, acct, body, clientIP, chatMeta, rc)
			defer cont.Close()
			stats := newChatStatsReaderSince(cont, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// 标记修复：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，
			// 这里在透传前还原成 delta.tool_calls（见 upstream/dsml.go）。
			// 第二个参数 true = 名单为空时启用弱判定——tools 声明在链路上丢失是
			// 实测最常见的泄漏成因，只做严格判定等于对主场景不设防。
			repair := upstream.NewMarkupRepair(declaredTools, true)
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}), upstream.WithMarkupRepair(repair), upstream.WithModelEcho(modelEcho))
			// 首帧否决（见 upstream.HeadVetoError）：网关一个字节都没写出，这次尝试对
			// 客户端完全无痕——按错误类型处置账号后换号，而不是把「HTTP 200 + error 帧」
			// 透给客户端（SSE 发过字节就改不了状态码，此前只能这么兜底）。
			var hv *upstream.HeadVetoError
			if errors.As(sErr, &hv) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeStreamError
				// 上游 body 是活的（200 长流），随轮转立即关闭，不留给函数末尾的 defer 堆 fd。
				rc.Close()
				fail(acct.UID)
				if hv.Payload == "" {
					// 零有效帧：上游缺陷，不是账号的问题（不罚号），但换号值得一试。
					lastErr = sErr
					log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (no byte written, rotating)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
				} else {
					// 帧内错误没有独立 HTTP 状态：末端映射只看 Kind，Status 仅作信封占位。
					kind := upstream.FrameKind(hv.Payload)
					h.applyErrorPolicy(acct.UID, kind, hv.Payload, bareModel, nil)
					lastErr = &upstream.Error{Kind: kind, Msg: hv.Payload}
					log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame before first byte kind=%s payload=%s (rotating)",
						logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(hv.Payload, 200))
				}
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（首帧否决换号退避）
				}
				continue
			}
			switch {
			case upstream.IsEmptyStreamError(sErr):
				// 上游 200 但空流（0 有效帧）：只有「注释/心跳行已先写出、响应头已提交」
				// 的残留形态能走到这里（首帧否决已把零字节形态换号了）。StreamHint 已写
				// error 帧 + [DONE] 兜底（只能 200），但这是上游缺陷不是成功——日志/
				// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
				// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			case sErr != nil:
				st.outcome = reqlog.OutcomeInterrupted
			case stats.SawErrorFrame():
				st.outcome = reqlog.OutcomeStreamError
			default:
				st.outcome = reqlog.OutcomeSuccess
			}
			credit, hasCredit := stats.Credit()
			total, hasTotal := stats.TotalTokens()
			// 计费字段合法性闸门（流式与聚合共用口径，见 validUsageCost）：只在
			// credit 显式存在且有限非负、total 为正整数时计入成本账本/用量账；
			// 缺字段/NaN/负值不再被当成「免费」或脏数据污染台账（上游 PR #104）。
			hasCredit = validUsageCost(credit, hasCredit, total, hasTotal)
			recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptStarted, stats.TTFB())
			if n, ok := stats.Thinking(); ok {
				st.think = n
			}
			if n := repair.Converted(); n > 0 {
				// 修复命中：上游本来会把工具调用吐成正文标记，这里已还原成 tool_calls。
				// 一行日志便于运维确认「这次不再空转」并统计发生率。
				log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, n, len(declaredTools))
			} else if m := repair.Seen(); m > 0 {
				// 识别到标记却没还原：判定没过（工具名不合规 / 块内夹带正文 / 参数畸形）。
				// 单独一行，排障时能一眼区分「没识别到」与「识别到但拒绝」。
				log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, m, len(declaredTools))
			}
			// issue #44：声明了 tools 却拿到 finish_reason=stop 且全流零 tool_calls ——
			// 上游把这个形态当正常收尾，客户端据此认为回合完成、工具一次没跑，用户
			// 只能手动催一句。此前网关侧完全无痕（这是定位困难的主因），这里只记日志
			// 并非把流改成错误：模型「判断不需要工具、直接回答」是合法行为，强行改写成
			// 错误会让这类正常回合反复重试、每次都真实计费。
			//
			// 级别是 INFO 不是 WARN：这是合法收尾，不是故障。记 WARN 会进「错误汇总」
			// 的兜底桶（其他（未归类）），把正常回合当成错误看待。
			if len(declaredTools) > 0 && !stats.SawToolCalls() && stats.FinishReason() == "stop" {
				log.Printf("INFO: [server] stream acct=%s model=%s: finish_reason=stop with zero tool_calls despite %d declared tool(s) (本轮无工具调用，正常收尾)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, len(declaredTools))
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			if toks, hasUsage := stats.Tokens(); hasUsage {
				st.toks = toks
			}
			st.cacheHit, st.cacheMiss, st.hasCache = stats.Cache()
			// WARN 信号（issue #92 同步）：曾命中过的模型本次大前缀整段未命中 →
			// 记一条可行动告警（上游前缀缓存重算，费用数倍放大）。
			if st.hasCache {
				cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, int64(st.cacheHit), int64(st.cacheMiss))
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if hasCredit {
				st.credit, st.hasCredit = credit, true
				h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			// 与流式空流那条 WARN 对称——此前这条只写响应不落日志，运维侧只能看到
			// 一行 sync 502，无从判断是上游空体还是别的（同一失败在流式路径有 WARN）。
			log.Printf("WARN: [server] sync acct=%s model=%s: upstream parse failed: %v", logfmt.Label(acct.UID, acct.Nickname), bareModel, err)
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		// 空截断救援（见 starved.go）：额度被思考吃光、正文零字节。非流式这一侧
		// 没有 continueReader，救援在这里做——同预算关思考重发一次、重新聚合。
		if aggregateStarved(resp) && bodyHasOutputLimit(body) {
			if rb, ok := starvedRetryBody(body); ok {
				if rc2, st2, _, e2 := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, rb, clientIP, chatMeta, nil); e2 == nil && st2 < 400 && rc2 != nil {
					if resp2, e3 := upstream.Aggregate(rc2); e3 == nil {
						rc2.Close()
						resp = resp2
						log.Printf("INFO: [starved] non-stream acct=%s model=%s: 预算被思考吃光（正文 0B）→ 同额度关思考重发成功",
							logfmt.Label(acct.UID, acct.Nickname), bareModel)
					} else {
						rc2.Close()
						log.Printf("WARN: [starved] non-stream acct=%s model=%s: 重发聚合失败: %v",
							logfmt.Label(acct.UID, acct.Nickname), bareModel, e3)
					}
				} else {
					if rc2 != nil {
						rc2.Close()
					}
					log.Printf("WARN: [starved] non-stream acct=%s model=%s: 关思考重发失败（status=%d err=%v）→ 保留原截断响应",
						logfmt.Label(acct.UID, acct.Nickname), bareModel, st2, e2)
				}
			}
		}
		// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
		// 只在「没有结构化 tool_calls」且「finish_reason 为 stop」时生效（见 dsml.go）；
		// 名单为空时走弱判定（tools 声明在中转环节丢失是常态）。
		mrepair := upstream.RepairAggregatedResponse(resp, declaredTools, true)
		if n := mrepair.Converted(); n > 0 {
			log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
				bareModel, n, len(declaredTools))
		} else if m := mrepair.Seen(); m > 0 {
			log.Printf("WARN: [server] non-stream model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
				bareModel, m, len(declaredTools))
		}
		ud := usageDeltaFromResponse(resp)
		credit, total, hasCredit := usageCreditTotal(resp)
		recordAttempt(acct.UID, ud, credit, hasCredit, attemptStarted, 0)
		// 模型名回显（issue #30，与流式 WithModelEcho 同语义）：聚合体的 model
		// 覆写回客户端原发名，严格校验客户端不当场报错。
		if modelEcho != "" {
			resp["model"] = modelEcho
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.toks = completionTokens(resp)
		if ud.HasCacheTokens {
			st.cacheHit, st.cacheMiss, st.hasCache = int(ud.CacheHitTokens), int(ud.CacheMissTokens), true
		}
		// WARN 信号（issue #92 同步）：与流式路径同口径（st.promptTokens 已由
		// recordAttempt 按本次观测填入）。
		if st.hasCache {
			cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, int64(st.cacheHit), int64(st.cacheMiss))
		}
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if hasCredit {
			st.credit, st.hasCredit = credit, true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	// 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	// requestId 原样保留）。HTTP 状态码按 OpenAI 兼容口径映射类别：ErrSoftRate → 429
	// （限流语义、客户端应等待重试），其余保持 503。本地调度类错误（无可用账号/
	// 传输层抖动/非上游返回的 lastErr）→ 保留自有文案 no_healthy_account（本地错误
	// 没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// quota：积分耗尽专用形态（429 + type/code 同为 insufficient_quota，OpenAI
	// 自己的 quota 错误口径；客户端据此不重试）。
	quota := false
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）走 noHealthyHint —— 裸名落错域时改写成带前缀的指引。
	hint := h.noHealthyHint(routeModel, realm, bareModel)
	// upstreamMsgPassed 记录 error.message 是否已被上游原文占据：模型级阻塞分支
	// 据此决定要不要覆盖 msg（上游原文优先，含 requestId）。
	upstreamMsgPassed := false
	// retryAfterSec 非 0 时写 Retry-After 头：只有「全池该模型只是忙/被限流」
	// 这种可重试形态才有意义（见下方 modelBlock 分支）。
	retryAfterSec := 0
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrUpstreamBusy:
			// 6021 渠道级容量满载（防御路径：正常已在轮转循环内 fail-fast，本分支
			// 只在异常流经末端时兜底）。503 + 可重试语义，上游原文（下方统一透传）
			// 含 requestId。
			status = http.StatusServiceUnavailable
			code = "upstream_busy"
			msg = "upstream capacity is full (queue.full); retry after a short wait"
		case upstream.ErrHardCredit:
			// 14018/402：积分耗尽。429 + insufficient_quota，message 优先上游
			// 原文（含购买链接；下方统一覆盖）。
			status, quota = http.StatusTooManyRequests, true
			msg = hardCreditMessage
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：网关出口 IP 被 WAF 拦截、
				// 轮转已止损、窗口过后自动解除。客户端提前重试无意义（换号不换 IP）；
				// 有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：上游原文（下方统一透传）已经写清了原因，但
			// code 此前停在默认的 no_healthy_account —— 那是"服务端过载"的语义，
			// 客户端据此会不断重试（#81 里就是 503 → 客户端自动重试 5 次），而真正
			// 该做的是换个模型。用 400 + 明确的 code 把不可重试的性质讲清楚。
			// （吸收上游 44d6e04）
			code = "model_unavailable"
			status = http.StatusBadRequest
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
			upstreamMsgPassed = true
		}
	}
	// 模型级阻塞覆盖上面的通用文案（上游原文优先，本地信息进 hint）。
	// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的
	// 上游原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上
	// 了上游不会告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。
	//
	// 用 400 而非 503：这是"你选的这个模型当前不可用"，不是"服务端暂时过载"。
	// 换号重试必然同样失败，客户端不该按可重试错误处理。
	// 积分耗尽细化（fork 自有）：与上面的 errors.As 分支互斥（无上游原文时才成立），
	// 且优先于模型级阻塞——池里一个号都试不出来时，「没积分」比「换模型」更准确。
	// 轮转耗尽 / WAF 止损等其它退出路径不经过选号失败分支，modelBlock 还是零值：
	// 在这里补一次快照。否则同一件「全池该模型只是忙」的事，走选号失败是
	// 503 model_busy + Retry-After，走轮转耗尽却是 429 rate_limit_exceeded 且没有
	// Retry-After（MaxRotate=1 时必现）。
	if !modelBlock.Blocked {
		modelBlock = h.cfg.Pool.ModelBlockedForRealm(curModel, realm)
	}
	if ue == nil && h.cfg.Pool != nil && h.cfg.Pool.AllHardCreditForRealm(realm) {
		// 池里一个可试的号都没有，且挡路的全是余额耗尽冷却：把笼统的「无可用账号」
		// 细化成积分耗尽语义——客户端对着必然失败的请求重试只是白等。
		status, quota = http.StatusTooManyRequests, true
		msg = hardCreditMessage
		hint = upstream.GatewayHint(upstream.ErrHardCredit, "", upstream.HintContext{})
	} else if modelBlock.Blocked && modelBlockIsTransient(modelBlock.Reason) {
		// 全池该模型只是「忙 / 被上游限流」（14003 model busy、6004 rate limit），
		// 不是「这个模型不存在或这个号型不允许」——上游对这两种的处置完全不同：
		// 忙是该等（官方客户端就是这么做的：查 /v2/chat/queue/status 排队），
		// 而模型级 11102/11158 是换模型才有用。
		//
		// 此前一律回 400 model_unavailable，客户端（DeepSeek Harness / Codex 等）
		// 据此判定「模型被杀、重试无意义」，高峰期就表现为「DeepSeek 用不了」，
		// 而官方客户端同一时刻还在排队等结果（issue #24）。这里改成可重试语义：
		// 503 + Retry-After（= 最早解封时刻）+ code=model_busy，客户端按头等待重试。
		code = "model_busy"
		status = http.StatusServiceUnavailable
		if !upstreamMsgPassed {
			msg = "model is temporarily busy on every account (per-model cooldown); retry after the cooldown window"
		}
		hint = fmt.Sprintf("model_busy: %d account(s) cooling down this model", modelBlock.Count)
		if !modelBlock.Until.IsZero() {
			if d := time.Until(modelBlock.Until); d > 0 {
				secs := int(math.Ceil(d.Seconds()))
				retryAfterSec = secs
				hint += fmt.Sprintf("; earliest unblock in %ds", secs)
			}
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
	} else if modelBlock.Blocked {
		code = "model_unavailable"
		// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号
		// 被挡、最早何时解封"这类本地调度信息放进 gateway_hint。
		if !upstreamMsgPassed {
			msg = "model is unavailable on every account (per-model cooldown), try another model"
		}
		hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
		if !modelBlock.Until.IsZero() {
			hint += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
		status = http.StatusBadRequest
	}
	if retryAfterSec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
	}
	if quota {
		writeOpenAIQuotaError(w, msg, hint)
	} else {
		writeOpenAIErrorHint(w, status, code, msg, hint)
	}
	st.status = status
	// 上游流级失败（首帧否决 / 流内 error 帧）的观测是 stream_error：客户端拿到 503
	// 只是「网关没能提交流」的结果，不是另一类故障，别在这里洗成 http_error。
	// 客户端主动断连（interrupted）同理：人走了不是网关的 HTTP 错误，别洗成
	// http_error 混进失败统计。
	if st.outcome != reqlog.OutcomeStreamError && st.outcome != reqlog.OutcomeInterrupted {
		st.outcome = reqlog.OutcomeHTTPError
	}
}

// hardCreditMessage 池内无号可试、原因全是积分耗尽时的自有文案（无上游原文可
// 透传的场合；与 gateway_hint 并列，不重复 hint 的「等签到恢复」指向）。
const hardCreditMessage = "all accounts have exhausted upstream credits; retry after credits are restored (daily check-in or add-on packs)"

// modelBlockIsTransient 判断模型级冷却的原因是「暂时忙/被限流」还是「确定性拒绝」。
//
// 依据是写冷却时存下的 Reason 原文（pool.CooldownModelBusy / CooldownSoftForModel
// 写入，来源就是上游 body）：
//   - 瞬时（可重试）：14003「模型繁忙」、6004「模型限流」；
//   - 确定性（换模型才有效）：11102「该后端无此模型」、11158「号型不允许」。
//
// 两者此前都回 400 model_unavailable，等于把高峰期的一次忙判成「模型永久不可用」，
// 客户端据此不重试——官方客户端同一时刻还在 /v2/chat/queue/status 排队（issue #24）。
func modelBlockIsTransient(reason string) bool {
	s := strings.ToLower(reason)
	if s == "" {
		return false
	}
	for _, marker := range []string{"14003", "6004", "model busy", "model rate limit", "rate limit"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// tokensPerSecond 计算吐字速率（token/s），返回 (速率, 是否有意义)。（吸收上游 8b18ab8）
//
// 分母用「生成耗时」= 端到端耗时 - 首 token 等待（TTFB），不是端到端耗时。
//
// 为什么要减：不减的话，首 token 等待越长、报告速率被压得越低。同一模型换个
// 上游或网络，TTFB 从 0.3s 涨到 2s，速率能凭空掉一半——读起来像"模型变慢了"，
// 其实只是排队久了（issue #34，维护者也确认速率计算没减首 token 到达时间）。
//
// ttfb<=0 表示没有观测：非流式回复天然没有「首个 data 帧」（日志里那一列记的是
// "-"），也因此拿不到「生成段」。此时**不报速率**（ok=false）：总时长里混着预填
// （实测 RikkaHub 那种 35 万 token 提示、60 个输出 token 的请求，端到端口径只有
// 16~90 tok/s，读起来像模型慢，其实全是在处理提示）。不报比报一个不可比的数诚实。
//
// 同理，ttfb 不小于总耗时时（时钟粒度、或 TTFB 落在计时终点之后）退回端到端耗时，
// 避免零/负分母。token 数为负哨兵值（-1 = 观测缺失）时返回 false。
//
// 扣除后不足 minGenWindow 也退回端到端（吸收上游 e05edb7，issue #127）：ttfb 量的是
// 「首个 SSE 帧到达」，当上游把整个响应攒到最后一次性下发（假流式/中间层攒批刷新——
// 首帧与末帧几乎同时到）时，total−ttfb 只剩几毫秒，拿它当分母会把几百 token 除成
// 上万 tok/s 的幻数。这种形态下「生成时长」根本不可测，诚实的分母只有端到端。
//
// 用量账本（handler）与控制台流水行（logging.go）都走这一个函数：两处各算一遍时
// 口径漂移过一次（流水行漏扣 TTFB、与面板数字对不上），共用是防再次分叉的唯一办法。
func tokensPerSecond(completionTokens int64, total, ttfb time.Duration) (float64, bool) {
	if completionTokens < 0 || total <= 0 || ttfb <= 0 {
		return 0, false
	}
	gen := total
	if g := total - ttfb; g >= minGenWindow {
		gen = g
	}
	return float64(completionTokens) / gen.Seconds(), true
}

// minGenWindow 可信生成窗口的下限：扣除 TTFB 后剩余窗口不足该值即视为「生成时长
// 不可测」，退回端到端耗时（见 tokensPerSecond 注释，issue #127）。真流式下首帧到
// 末帧通常铺满剩余窗口，200ms 远低于正常生成时长，不影响真实快速输出（缓存命中后的
// 爆发）被如实报告。
const minGenWindow = 200 * time.Millisecond

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// usageCreditTotal 从聚合响应取 usage.credit 与 total_tokens（成本台账非流式入口）。
// 任一字段缺失/非法 → ok=false（不记录）。移植上游 PR #104。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, false
	}
	c, hasCredit := usage["credit"].(float64)
	t, hasTotal := usage["total_tokens"].(float64)
	parsedTotal, validTotal := parseUsageTotalTokens(t)
	if !validTotal {
		return 0, 0, false
	}
	if !validUsageCost(c, hasCredit, parsedTotal, hasTotal) {
		return 0, 0, false
	}
	return c, parsedTotal, true
}

// validUsageCredit 只接受「显式存在、有限、非负」的 credit：0 合法（= 上游确认免费）。
func validUsageCredit(credit float64, present bool) bool {
	return present && !math.IsNaN(credit) && !math.IsInf(credit, 0) && credit >= 0
}

// parseUsageTotalTokens 只接受落在 int 范围的有限正整数（小数/越界/NaN 一律拒绝）。
func parseUsageTotalTokens(total float64) (int, bool) {
	if math.IsNaN(total) || math.IsInf(total, 0) || total <= 0 || math.Trunc(total) != total {
		return 0, false
	}
	parsed := int(total)
	if parsed <= 0 || float64(parsed) != total {
		return 0, false
	}
	return parsed, true
}

// validUsageCost 计费闸门（流式与聚合共用）：credit 合法且 total 为有效正整数。
func validUsageCost(credit float64, hasCredit bool, total int, hasTotal bool) bool {
	return validUsageCredit(credit, hasCredit) && hasTotal && total > 0
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：第 i 次轮转失败
// （continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），
// ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转（客户端已走，
// 换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；正常单号请求
// （首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避。冷却时长优先采信
//     Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却：**不 Disable**——WAF 403 是 IP/指纹维频控信号，
//     罚过即走、到期自愈。时长优先 Retry-After 头；缺失按 wafCooldownBase(60s)
//     起 · softStreak 指数、封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避。
//     基数经 jitterDur 抖动（防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号；passthrough 首遇触发降级重试，最终仍拦则回 400。
//   - ErrPromptTooLong → 11115：请求的问题不是账号的问题。零动作（不冷却/不熔断/
//     不 NoteError、不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrBadParams → 11101：不罚账号，且与 ErrPromptTooLong/ErrImageInvalid 同待遇：
//     不冷却/不熔断/不 NoteError/不喂连败；换号必然同样失败，故 chatCompletions
//     在轮转循环内即刻 400 透传上游原文终止（吸收上游 fd5c5a6）。
//   - ErrModelParamInvalid → 11133：同上（请求级），零动作 + 循环内即刻 400 终止。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102/11158 负缓存避让。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池。
//
// body 仅在 ErrSoftRate/ErrAccountFault 分支用于解析重置时间/分野；model 为请求
// 携带的模型名。uerr 是 ChatStreamContext 返回的分类信封（可携带 RetryAfter）；
// 零值/防御路径下为 nil，冷却时长回落既有计算。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 统一对齐上游重置时间：只要 body 带「将在 … 重置」，就精确冷却到该墙钟、绝不
		// 指数堆加。
		//   - 模型级（6004 / 14003）→ CooldownSoftForModel：写 modelCooldowns[model]，
		//     切模型豁免。**无论有没有解析时间都走这条**——模型忙不是账号坏，按账号冷却
		//     会把一个模型的抖动放大成整池停摆（实测同号换模型立刻可用）。
		//   - 账号级（其余 429 文案）→ CooldownSoftRate：写账号级 until。
		modelRateLimited := upstream.IsModelRateLimit(body)
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if modelRateLimited {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
			return
		}
		// body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加）。
		// 头优先于「有界退避」，但低于 body 重置文案（文案是上游更权威的口径）。
		retryAfter := time.Time{}
		if uerr != nil && uerr.RetryAfter > 0 {
			retryAfter = time.Now().Add(uerr.RetryAfter)
		}
		if modelRateLimited {
			// 14003 是「模型繁忙」：没有恢复时刻，上游文案就是让客户端换模型/稍后
			// 重试，不是这个号被限流。给独立短窗口（10s 起、封顶 1m），不套 soft_rate
			// 的 600s→2h 阶梯，否则一次抖动就是十分钟不可用。
			if retryAfter.IsZero() && upstream.IsModelBusy(body) {
				h.cfg.Pool.CooldownModelBusy(uid, model, "14003 model busy")
				return
			}
			h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), retryAfter, model, "model rate limit")
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max；已在冷却中的兜底探测不翻倍）。基数取 h.softCooldown()
		// （热改优先），管理面板改 soft_rate 后立即生效。
		reason := "429 rate limit"
		if !retryAfter.IsZero() {
			reason = "429 rate limit (retry-after)"
		}
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), retryAfter, reason)
	case upstream.ErrWafBlock:
		// WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族：基数
		// wafCooldownBase（60s，抖动后落 [45s,75s]）、softStreak 指数升级、封顶
		// soft_rate_max、冷却中兜底探测不翻倍——全部继承既有语义。
		// Retry-After 头优先（WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		// 两种形态同属「session 已死，只能重登」，但成因不同、处置强度也不同：
		//   - 业务码 12153（refresh 端点/应用层明说 offline session 失效）→ 一次即禁。
		//   - 网关 401 invalid_token（APISIX HTML 页，无业务信封）→ **连续阈值** 才禁。
		//     理由同 pool.NoteSessionDead 的误杀复盘：边缘节点抖动也会回 401 HTML，
		//     一次就禁会在上游抖一下时把整池打光（这个坑本项目踩过一次）。死号在阈值
		//     内仍会被选中重试，但最多 3 次即出池，不再每请求刷 401。
		if uerr != nil && upstream.IsGatewayUnauthorized(uerr.Status, body) {
			// 先 refresh 求证再计死：网关 401 有两种成因，响应体完全一样（APISIX/
			// openresty 的 HTML 页，无业务信封），单看 401 分不出来——
			//   a) token 真失效 → refresh 被拒 → 该计死；
			//   b) 网关瞬时拦截（风控/边缘抖动）→ refresh 仍成功 → 不该罚。
			// 实测（2026-10-24 issue #66）：两号各累计 3 次 401 被禁，revive 后立刻
			// 全 200，即情形 b。不清零计数，多次瞬时拦截会累积到阈值误杀健康号。
			// 与 balance_401.go 同一套求证逻辑（那里已验证过）。
			//
			// 代价：每个网关 401 多一次 refresh 往返；refresh 轻量且 401 低频，另由下面
			// 的软冷却兜住「同一账号每请求都来问一次」。
			if acct := h.cfg.Pool.AuthByUID(uid); acct != nil && h.cfg.Upstream != nil {
				switch h.cfg.Upstream.VerifySessionByRefresh(acct) {
				case upstream.VerifyAlive:
					// 上游已轮换 RT：必须落盘，否则重启拿旧 RT 刷新 = 账号真死。
					if serr := acct.SaveAtomicRetry(); serr != nil {
						log.Printf("ERR: [server] 网关 401 后 refresh 成功但落盘失败: %v", serr)
					}
					h.cfg.Pool.ClearSessionDead(uid)
					// 用 CooldownSoftRate 而非固定 Cooldown：60s 起按 softStreak 翻倍
					// （封顶 soft_rate_max），任何一次成功即清零。防「chat 恒 401 但
					// refresh 恒成功」的降级 token 每 60s 轮换一次 RT（1440 次/天）。
					h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "gateway 401 (refresh ok, transient)")
					log.Printf("INFO: [server] 账号 %s 网关 401 但 refresh 成功 → 瞬时拦截，不计 session dead",
						logfmt.Label(uid, ""))
					return
				case upstream.VerifyInconclusive:
					// refresh 未给出明确判词（网络/5xx/429/refresh 端点自身被拦）：
					// 既不能判活也不能判死，只短退避。判死会在同一层再误杀一次。
					h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "gateway 401 (refresh inconclusive)")
					log.Printf("INFO: [server] 账号 %s 网关 401，refresh 未给出明确判词 → 不计 session dead",
						logfmt.Label(uid, ""))
					return
				}
				// VerifyDead：落到下面的计死逻辑。
			}
			// reason 带上「次数 + 已 refresh 验证」两个事实：面板只显示这一行，
			// 运维要能一眼判断是「刚发生」还是「反复出现」，以及「是否已排除网关抖动」。
			if h.cfg.Pool.NoteSessionDeadWith(uid, fmt.Sprintf(
				"网关 401 × %d 且 refresh 验证失败（token 已确认失效）", pool.SessionDeadThreshold())) {
				log.Printf("WARN: [server] 账号 %s 连续 %d 次网关 401 且 refresh 失败（token 确实失效）→ 禁用，需重新登录",
					logfmt.Label(uid, ""), pool.SessionDeadThreshold())
			}
			return
		}
		h.cfg.Pool.Disable(uid, "上游 12153 session dead（业务码明说登录态失效，需重新登录）")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal" + code "11-128"（**渠道校验**）→ 上游自己的 displayTips 写着
		//     「请求被拦截，请重新发送」「渠道校验失败，可能不是从官方客户端发出的」：请求级
		//     可重试信号（实测凭证/余额/JWT 全正常、换模型换头不改结果、同号在别的客户端可用）。
		//     永久禁用等于把好号踢出池子还要人工解冻 → 软冷却，上游解除后自愈。
		//   - "request illegal" + code 11140（数字）→ auth_forbidden 账号级授权风控：硬禁用。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			if upstream.HasCodeMarker(body, "11-128") {
				h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{},
					"channel verification blocked (11-128 request illegal)")
				return
			}
			h.cfg.Pool.Disable(uid, "account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked 待遇），
		// chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备。
	case upstream.ErrImageInvalid:
		// 图片格式/数据无效：请求的问题不是账号的问题（同一 body 换任何号都会
		// 得到相同解析错误）。零动作，chatCompletions 已 fail-fast 透传。
	case upstream.ErrUpstreamBusy:
		// 6021「queue.full.title」渠道级容量满载：账号余额健康、未限流、session
		// 未死，换号也撞同一堵墙。零动作（不冷却/不熔断/不 NoteError/不喂连败，
		// 同 ErrContentBlocked 待遇）——一旦按 ErrSoftRate 罚号，会与全冷却兜底
		// 互相喂成整池停摆（2026-10-15 实案）。chatCompletions 已 fail-fast 503
		// 透传原文、终止轮转——该分支只为文档完备。
	case upstream.ErrToolMismatch:
		// 11148 工具配对断裂：请求内容的问题（会话历史里的 tool_calls / tool 结果对不上），
		// 同一 body 换任何账号都是同样的拒绝。零动作（不冷却/不熔断/不 NoteError/不喂
		// 连败，同 ErrBadParams 待遇）；chatCompletions 已 fail-fast 400 透传原文、
		// 终止轮转——该分支只为文档完备。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关侧不再截断，均为客户端畸形 JSON）。换上任何账号照样 400，
		// 故零动作（无冷却/熔断/NoteError/连败喂养）；终止轮转与 400 透传原文由
		// chatCompletions 的 ErrBadParams 分支负责（吸收上游 fd5c5a6）。
	case upstream.ErrModelParamInvalid:
		// 11133「参数被模型供应商拒绝」（缺/空 messages、模型不支持图片等）：请求级
		// 错误，与 ErrBadParams/ErrPromptTooLong/ErrImageInvalid 同待遇——零动作
		// （不冷却/不熔断/不 NoteError/**不喂连败计数**：连败降权是 ErrClient 这种
		// 「不知道原因的失败」的兜底，有权威分类的请求级错误不该重复计罚）。
		// 终止轮转与 400 透传原文由 chatCompletions 的请求级参数分支负责。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」/11158「该账号类型无此模型权限」：
		// (账号, 模型) 负缓存避让。复用 modelCooldowns 机制（与 6004 同域），
		// 选号侧 healthyForModel 对该账号自动避开该模型。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReasonFor(body))
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// wafCooldownBase WAF 403 软冷却基数（建议 60s 起；抖动 ±25% 后落 [45s,75s]，
// 实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控，信号比 429「账号级限流」轻
// （账号本身健康），但比 404 重（带粘性会连环）；60s 级的快速避让已足够让频控窗口
// 滑过。抖动复用 backoff.go jitterDur（单一来源）。
const wafCooldownBase = 60 * time.Second

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// writeOpenAIQuotaError 积分耗尽专用响应：429 + type/code 同为 insufficient_quota
// （OpenAI 自身 quota 错误的字段口径，客户端按 type 或 code 任一即可识别「不是限流、
// 重试无意义」）。message 照旧装上游原文或自有文案。
func writeOpenAIQuotaError(w http.ResponseWriter, msg, hint string) {
	e := map[string]any{
		"message": msg,
		"type":    "insufficient_quota",
		"code":    "insufficient_quota",
	}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": e})
}

// toolNamesIfPresent 是 ToolNameAllowlist 的廉价前置闸门：body 里没有 "tool
// 或 "function 这类键前缀时不可能出现工具名，直接返回 nil（与解析结果为空的
// 语义一致）。文本里偶然出现这些词只会多解析一次，不改变结果。
func toolNamesIfPresent(body []byte) map[string]bool {
	if !bytes.Contains(body, []byte(`"tool`)) && !bytes.Contains(body, []byte(`"function`)) {
		return nil
	}
	return upstream.ToolNameAllowlist(body)
}

// imagePartIfPresent 是 hasImagePart 的前置闸门：hasImagePart 只认 content
// part 的 type == "image_url"，它必然以 "image_url" 子串出现；没有该子串时
// 跳过整段解析。
func imagePartIfPresent(body []byte) bool {
	return bytes.Contains(body, []byte(`"image_url"`)) && hasImagePart(body)
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"}）。畸形/其他形态一律 false（hint 侧
// 宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		// Text-only system/history messages use strings; tool-call messages
		// may use null. Neither should hide images in another message.
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 10min 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			ctx.ModelSupportsImagesKnown = mi.SupportsImagesSet
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
// noHealthyHint 选号失败（no_healthy_account）的 gateway_hint。
//
// 裸模型名默认落 cn 域：池里若只有另一域的号，选号必然失败，而固定文案指向的
// /status 永远是健康的——客户端据此只会一直重试，看不到真正该改的是模型名。
// 这里在「请求确实是裸名」且「另一域有该模型的健康号」时，改写成可操作的带前缀
// 写法；其余情况一律退回固定文案（不编造能力事实）。
//
// 显式带前缀的请求不做改写：前缀不是它失败的原因，建议换域反而误导。
func (h *Handler) noHealthyHint(routeModel, realm, bareModel string) string {
	if routeModel != bareModel {
		return upstream.NoHealthyAccountHint()
	}
	other := "global"
	if realm == "global" {
		other = "cn"
	}
	if len(h.cfg.Pool.AvailableUIDsForModelRealm(bareModel, other)) > 0 {
		return upstream.NoHealthyAccountHintForModel(bareModel, other)
	}
	return upstream.NoHealthyAccountHint()
}

func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}
