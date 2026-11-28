package ratelimit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// UsageRow 是注入的「窗口内某账号在某模型上的消耗」。
type UsageRow struct {
	Realm    string
	UID      string
	Model    string
	Tokens   int64
	Requests int64
	Errors   int64
	Credits  float64
	// FirstAt 是窗口内最早一笔消耗的时间（整点桶的起点，近似值）。
	//
	// 上游的滚动窗口从「该号该模型第一次请求」起算，不是自然日重置；拿到这个时刻
	// 才能算出窗口什么时候翻篇（见 Row.RetryAfterSec）。用量只有整点粒度，所以它是
	// 近似值——误差在一小时内，够用来给客户端一个 Retry-After 提示。
	FirstAt time.Time
}

// RateLimit 是账号在某模型上的上游限流记录（池的模型级冷却台账的投影）。
type RateLimit struct {
	Model string
	Kind  string
	// Reason 是上游给出的限流原因（池的模型级冷却台账里的原文）。
	// 丢掉它，页面就只能显示「已限流」，看不出是配额耗尽还是别的。
	Reason  string
	Until   time.Time
	ResetAt time.Time
}

// Account 是账号快照（池 Status 的精简投影）：只带额度口径要看的字段。
type Account struct {
	UID      string
	Nickname string
	Realm    string
	Credits  float64 // 剩余积分（池内缓存值）

	Cooling  bool
	Disabled bool
	Paused   bool

	RateLimited []RateLimit
}

// Deps 是构建快照需要的外部数据，全部按「调用即取」闭包注入。
//
// 任何一项为 nil 都按空处理：面板先起来、池还没加载完时页面也要能打开，
// 请求路径也不能因为依赖缺失而 panic。
type Deps struct {
	Usage    func(window time.Duration) []UsageRow
	Accounts func() []Account
	Realms   func() []string
	Rates    func(realm string) map[string]string
	Models   func(realm string) []string
	Fallback func() map[string]string
	// Hidden 返回该域被 config.models.hidden 拉黑的模型集合（裸名）。
	// 与网关的 hiddenModel() 读同一份 livecfg.HiddenModels，热生效、大小写敏感。
	// 拉黑的模型不出现在页面、不进降级链、不参与请求路径判定。
	Hidden func(realm string) map[string]bool
	Now    func() time.Time
}

// View 是页面的整份快照，也是请求路径判定的事实来源。
type View struct {
	Generated time.Time               `json:"generated"`
	Policy    Policy                  `json:"policy"`
	Presets   []Preset                `json:"presets"`
	Realms    []string                `json:"realms"`
	Summary   map[string]RealmSummary `json:"summary"`
	Totals    Totals                  `json:"totals"`
	Rows      []Row                   `json:"rows"`
	Notes     []string                `json:"notes,omitempty"`
}

// Totals 是页头汇总卡的数字。
type Totals struct {
	Models    int     `json:"models"`
	Limited   int     `json:"limited_models"`
	Degrading int     `json:"degrading_models"`
	Accounts  int     `json:"accounts"`
	Routable  int     `json:"routable"`
	Credits   float64 `json:"credits"`
	Tokens    int64   `json:"tokens"`
	Requests  int64   `json:"requests"`
}

// RealmSummary 是按域汇总。
type RealmSummary struct {
	Accounts  int     `json:"accounts"`
	Routable  int     `json:"routable"`
	Credits   float64 `json:"credits"`
	Models    int     `json:"models"`
	Limited   int     `json:"limited_models"`
	Degrading int     `json:"degrading_models"`
}

// Usage 是某行在窗口内的消耗合计。
type Usage struct {
	Tokens   int64   `json:"tokens"`
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	Credits  float64 `json:"credits"`
}

// PoolUsage 是池级（= 该域参与选号的账号合计）的额度占用。
type PoolUsage struct {
	QuotaTokens      int64   `json:"quota_tokens"`
	UsedTokens       int64   `json:"used_tokens"`
	RemainingTokens  int64   `json:"remaining_tokens"`
	Pct              float64 `json:"pct"`
	QuotaCredits     float64 `json:"quota_credits"`
	UsedCredits      float64 `json:"used_credits"`
	RemainingCredits float64 `json:"remaining_credits"`
	CreditPct        float64 `json:"credit_pct"`
}

// AccountRow 是逐账号明细：这是「额度精确到账号」的落点。
type AccountRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	// Routable=false 的账号不参与池级额度累加：禁用/暂停号那份额度用不上。
	Routable bool    `json:"routable"`
	Cooling  bool    `json:"cooling,omitempty"`
	Disabled bool    `json:"disabled,omitempty"`
	Paused   bool    `json:"paused,omitempty"`
	Tokens   int64   `json:"tokens"`
	Credits  float64 `json:"credits"`
	Requests int64   `json:"requests"`
	// CreditsLeft 是账号剩余积分（钱还在账上，与「额度用掉多少」是两个维度）。
	CreditsLeft     float64 `json:"credits_left"`
	QuotaTokens     int64   `json:"quota_tokens"`
	RemainingTokens int64   `json:"remaining_tokens"`
	Pct             float64 `json:"pct"`
	// Limited 表示该账号此刻在这个模型上已经不接单了（额度耗尽或上游冷却）。
	Limited   bool   `json:"limited"`
	LimitKind string `json:"limit_kind,omitempty"`
	// Reason 是上游给的限流原因（仅上游冷却时有值）。
	Reason  string `json:"reason,omitempty"`
	Until   string `json:"until,omitempty"`
	ResetAt string `json:"reset_at,omitempty"`
	// QuotaResetAt 是该账号在这个模型上的窗口边界（最早用量 + 窗口长度，近似值）。
	QuotaResetAt string `json:"quota_reset_at,omitempty"`
}

// Row 是一个 (域, 模型) 的限流行。
type Row struct {
	Realm     string  `json:"realm"`
	Model     string  `json:"model"`
	FullName  string  `json:"full_name"`
	Rate      string  `json:"rate,omitempty"`
	RateValue float64 `json:"rate_value"`
	HasRate   bool    `json:"has_rate"`
	ZeroRate  bool    `json:"zero_rate"`

	Source    string `json:"source"`
	OnHit     string `json:"on_hit"`
	ChainMode string `json:"chain_mode"`
	WindowSec int64  `json:"window_sec"`

	Quota    Quota        `json:"quota"`
	Usage    Usage        `json:"usage"`
	Pool     PoolUsage    `json:"pool"`
	Accounts []AccountRow `json:"accounts"`

	LimitState       string `json:"limit_state"`
	LimitedAccounts  int    `json:"limited_accounts"`
	RoutableAccounts int    `json:"routable_accounts"`
	// QuotaResetAt 是池内最早有额度滚出窗口的时刻（近似值，见 AccountRow.QuotaResetAt）。
	QuotaResetAt string `json:"quota_reset_at,omitempty"`
	// RetryAfterSec 是给客户端的建议重试秒数（只在已吃紧时给出）。
	RetryAfterSec int `json:"retry_after_sec,omitempty"`

	Chain       []Step `json:"chain"`
	ChainSource string `json:"chain_source"`
	Degrading   bool   `json:"degrading"`

	Note string `json:"note,omitempty"`
}

// Build 把策略与外部数据算成页面快照。
//
// 全是纯函数：只读 Deps，不写任何状态。请求路径的判定（DecisionOf）与页面文案读
// 同一份结果——页面显示「已用 92%」时请求路径也真的在 90% 之后开始改道，否则这个
// 页面就只是装饰。
func Build(p Policy, d Deps) View {
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	accounts := []Account{}
	if d.Accounts != nil {
		accounts = d.Accounts()
	}
	ratesFor := func(realm string) map[string]string {
		if d.Rates == nil {
			return nil
		}
		return d.Rates(realm)
	}
	modelsFor := func(realm string) []string {
		if d.Models == nil {
			return nil
		}
		return d.Models(realm)
	}
	fallback := map[string]string{}
	if d.Fallback != nil {
		fallback = d.Fallback()
	}
	hiddenFor := func(realm string) map[string]bool {
		if d.Hidden == nil {
			return nil
		}
		return d.Hidden(realm)
	}

	usageCache := map[int64]map[string]UsageRow{}
	usageFor := func(windowSec int64) map[string]UsageRow {
		if cached, ok := usageCache[windowSec]; ok {
			return cached
		}
		agg := map[string]UsageRow{}
		if d.Usage != nil {
			for _, row := range d.Usage(time.Duration(windowSec) * time.Second) {
				key := usageKey(row.Realm, row.UID, row.Model)
				cur := agg[key]
				cur.Realm, cur.UID, cur.Model = normalizeRealm(row.Realm), row.UID, strings.TrimSpace(row.Model)
				cur.Tokens += row.Tokens
				cur.Requests += row.Requests
				cur.Errors += row.Errors
				cur.Credits += row.Credits
				if !row.FirstAt.IsZero() && (cur.FirstAt.IsZero() || row.FirstAt.Before(cur.FirstAt)) {
					cur.FirstAt = row.FirstAt
				}
				agg[key] = cur
			}
		}
		usageCache[windowSec] = agg
		return agg
	}

	realms := collectRealms(d, accounts)
	view := View{Generated: now, Policy: p, Presets: Presets(), Realms: realms, Summary: map[string]RealmSummary{}}
	if len(fallback) > 0 {
		view.Notes = append(view.Notes, "已接入池的 model_fallback 表：没有人工链的模型会按该表降级。")
	}
	inferred := false

	for _, realm := range realms {
		realmAccounts := make([]Account, 0, len(accounts))
		routable := 0
		var credits float64
		for _, a := range accounts {
			if normalizeRealm(a.Realm) != realm {
				continue
			}
			realmAccounts = append(realmAccounts, a)
			credits += a.Credits
			if !a.Disabled && !a.Paused {
				routable++
			}
		}
		sort.SliceStable(realmAccounts, func(i, j int) bool { return realmAccounts[i].UID < realmAccounts[j].UID })

		rateTable := ratesFor(realm)
		// drop 一次算好两类「不该出现在页面上」的模型：运营拉黑的黑名单，加上客户端
		// 折叠出来的分类汇总别名（deep-model / balanced-model 之类，见 aliases.go）。
		// 倍率表也要过一遍 —— 它同时是自动降级链的候选池。
		drop := dropFor(hiddenFor(realm))
		rateTable = filterRates(rateTable, realm, drop)
		models := collectModels(realm, modelsFor(realm), p, usageFor(DefaultWindowSec))
		models = filterModels(models, realm, drop)

		rows := make([]Row, 0, len(models))
		sum := RealmSummary{Accounts: len(realmAccounts), Routable: routable, Credits: credits}
		for _, model := range models {
			limit := EffectiveLimit(p, realm, model)
			quota := ResolveQuota(limit, realm, routable)
			// 跟随上游的行才按上游反馈自适应：手动配额是运营的明确决定，不能被信号改写。
			if quota.Inferred {
				quota = adaptQuota(quota, realm, model, routable, usageFor(limit.WindowSec), realmAccounts, now)
			}
			usage := usageFor(limit.WindowSec)
			rate := rateTable[model]
			rateVal := ParseRate(rate)
			if quota.Inferred {
				inferred = true
			}

			row := Row{
				Realm: realm, Model: model, FullName: realm + ":" + model,
				Rate: rate, RateValue: rateVal, HasRate: rateVal >= 0, ZeroRate: rateVal == 0,
				Source: limit.Source, OnHit: limit.OnHit, ChainMode: limit.ChainMode, WindowSec: limit.WindowSec,
				Quota: quota, Note: limit.Note, RoutableAccounts: routable,
			}

			limitedRoutable := 0
			for _, a := range realmAccounts {
				u := usage[usageKey(realm, a.UID, model)]
				row.Usage.Tokens += u.Tokens
				row.Usage.Requests += u.Requests
				row.Usage.Errors += u.Errors
				row.Usage.Credits += u.Credits

				ar := AccountRow{
					UID: a.UID, Nickname: a.Nickname, Cooling: a.Cooling, Disabled: a.Disabled, Paused: a.Paused,
					Tokens: u.Tokens, Credits: u.Credits, Requests: u.Requests, CreditsLeft: a.Credits,
					QuotaTokens: quota.TokenPerAccount,
				}
				ar.Routable = !a.Disabled && !a.Paused
				ar.RemainingTokens = quota.TokenPerAccount - u.Tokens
				if ar.RemainingTokens < 0 {
					ar.RemainingTokens = 0
				}
				ar.Pct = pct(float64(u.Tokens), float64(quota.TokenPerAccount))
				if rl, ok := accountRateLimit(a.RateLimited, model, now); ok {
					ar.Limited = true
					ar.LimitKind = "upstream"
					if rl.Kind != "" {
						ar.LimitKind = rl.Kind
					}
					ar.Reason = rl.Reason
					if !rl.Until.IsZero() {
						ar.Until = rl.Until.UTC().Format(time.RFC3339)
					}
					if !rl.ResetAt.IsZero() {
						ar.ResetAt = rl.ResetAt.UTC().Format(time.RFC3339)
					}
				}
				if quota.TokenPerAccount > 0 && u.Tokens >= quota.TokenPerAccount {
					ar.Limited = true
					if ar.LimitKind == "" {
						ar.LimitKind = "tokens"
					}
				}
				// 窗口边界：该号该模型在窗口内最早一笔消耗 + 窗口长度。
				//
				// ⚠️ 这是**近似**：用量只有整点桶粒度，且上游窗口是「从该号该模型第一次
				// 请求起算的滚动 24h」，不是自然日重置 —— 连续用量的账号窗口起点会一直
				// 往前挪。所以这个时刻只说明「最早有多少额度会滚出窗口」，不等于
				// 「额度到点就恢复」。页面要照这个口径说话（见 internal/ratelimit/README.md）。
				if !u.FirstAt.IsZero() && limit.WindowSec > 0 {
					ar.QuotaResetAt = u.FirstAt.Add(time.Duration(limit.WindowSec) * time.Second).UTC().Format(time.RFC3339)
				}
				if ar.Limited {
					row.LimitedAccounts++
					if ar.Routable {
						limitedRoutable++
					}
				}
				if ar.Routable {
					row.Pool.UsedTokens += u.Tokens
					row.Pool.UsedCredits += u.Credits
				}
				row.Accounts = append(row.Accounts, ar)
			}
			sort.SliceStable(row.Accounts, func(i, j int) bool {
				a, b := row.Accounts[i], row.Accounts[j]
				if a.Limited != b.Limited {
					return a.Limited
				}
				if a.Pct != b.Pct {
					return a.Pct > b.Pct
				}
				return a.UID < b.UID
			})

			row.Pool.QuotaTokens = quota.TokenPerPool
			row.Pool.RemainingTokens = quota.TokenPerPool - row.Pool.UsedTokens
			if row.Pool.RemainingTokens < 0 {
				row.Pool.RemainingTokens = 0
			}
			row.Pool.Pct = pct(float64(row.Pool.UsedTokens), float64(quota.TokenPerPool))
			row.Pool.QuotaCredits = quota.CreditPerPool
			row.Pool.RemainingCredits = quota.CreditPerPool - row.Pool.UsedCredits
			if row.Pool.RemainingCredits < 0 {
				row.Pool.RemainingCredits = 0
			}
			row.Pool.CreditPct = pct(row.Pool.UsedCredits, quota.CreditPerPool)

			row.LimitState = limitState(routable, limitedRoutable, row.Pool.Pct, row.Pool.CreditPct)
			// 行级窗口边界：取可路由账号里最早的那个 —— 那是第一笔额度滚出窗口的时刻。
			// 只在已经吃紧（limit_state != ok）时才对客户端有意义，所以 Retry-After
			// 也只在这时给；没吃紧还给重试时刻，会让客户端以为要等。
			if limit.WindowSec > 0 {
				var earliest time.Time
				for _, ar := range row.Accounts {
					if !ar.Routable || ar.QuotaResetAt == "" {
						continue
					}
					t, perr := time.Parse(time.RFC3339, ar.QuotaResetAt)
					if perr != nil {
						continue
					}
					if earliest.IsZero() || t.Before(earliest) {
						earliest = t
					}
				}
				if !earliest.IsZero() {
					row.QuotaResetAt = earliest.UTC().Format(time.RFC3339)
					if row.LimitState != "ok" {
						secs := int(earliest.Sub(now).Seconds())
						if secs < 1 {
							secs = 1
						}
						if int64(secs) > limit.WindowSec {
							secs = int(limit.WindowSec)
						}
						row.RetryAfterSec = secs
					}
				}
			}
			candidates := filterModels(modelsFor(realm), realm, drop)
			row.Chain, row.ChainSource = Chain(limit, realm, model, rateTable, candidates, fallback)
			row.Chain = filterSteps(row.Chain, realm, drop)
			if len(row.Chain) == 0 {
				row.ChainSource = ChainSourceNone
			}
			row.Degrading = row.LimitState != "ok" && limit.OnHit == OnHitDegrade && len(row.Chain) > 0

			sum.Models++
			if row.LimitState == "locked" {
				sum.Limited++
			}
			if row.Degrading {
				sum.Degrading++
			}
			rows = append(rows, row)
		}

		sortRows(rows)
		view.Rows = append(view.Rows, rows...)
		view.Summary[realm] = sum
		view.Totals.Accounts += sum.Accounts
		view.Totals.Routable += sum.Routable
		view.Totals.Credits += sum.Credits
		view.Totals.Models += sum.Models
		view.Totals.Limited += sum.Limited
		view.Totals.Degrading += sum.Degrading
	}
	for _, row := range view.Rows {
		view.Totals.Tokens += row.Usage.Tokens
		view.Totals.Requests += row.Usage.Requests
	}
	if inferred {
		view.Notes = append(view.Notes, "标注「推算」的行：token 配额取自上游已知口径（CN 200M / global 100M 每账号每模型 24h 滚动），不是本地合同值。")
	}
	if len(view.Rows) == 0 {
		view.Notes = append(view.Notes, "还没有可展示的模型：上游目录尚未刷新完成，或该域没有配置账号。")
	}
	return view
}

// Preview 是「这份策略如果生效会怎样」的预览（不落盘、不影响请求路径）。
type Preview struct {
	Realm       string     `json:"realm"`
	Model       string     `json:"model"`
	Limit       ModelLimit `json:"limit"`
	Quota       Quota      `json:"quota"`
	Pool        PoolUsage  `json:"pool"`
	LimitState  string     `json:"limit_state"`
	Chain       []Step     `json:"chain"`
	ChainSource string     `json:"chain_source"`
	Degrading   bool       `json:"degrading"`
	Routable    int        `json:"routable_accounts"`
	Rationale   []string   `json:"rationale"`
}

// PreviewWith 用给定策略算某 (域, 模型) 的预览。行不存在时返回说明而不报错：
// 页面在目录刷新完成前点预览不该弹红框。
func PreviewWith(p Policy, d Deps, realm, model string) Preview {
	realm = normalizeRealm(realm)
	model = strings.TrimSpace(model)
	if err := Validate(NormalizePolicy(p)); err != nil {
		return Preview{Realm: realm, Model: model, Rationale: []string{"策略不合规，无法预览：" + err.Error()}}
	}
	v := Build(p, d)
	for _, row := range v.Rows {
		if row.Realm != realm || !strings.EqualFold(row.Model, model) {
			continue
		}
		return Preview{
			Realm: realm, Model: row.Model, Limit: EffectiveLimit(p, realm, row.Model),
			Quota: row.Quota, Pool: row.Pool, LimitState: row.LimitState,
			Chain: row.Chain, ChainSource: row.ChainSource, Degrading: row.Degrading,
			Routable: row.RoutableAccounts, Rationale: previewRationale(row, realm),
		}
	}
	out := Preview{Realm: realm, Model: model, Limit: EffectiveLimit(p, realm, model)}
	out.Rationale = []string{"该模型当前不在目录里（可能上游目录还没刷新，或这个域没有账号），无法推算池级占用与自动链。"}
	return out
}

// NormalizePolicy 归一化整份策略（默认项 + 每个覆盖项）。
func NormalizePolicy(p Policy) Policy {
	out := p
	out.Preset = strings.ToLower(strings.TrimSpace(out.Preset))
	out.Default = Normalize(out.Default)
	overrides := make([]ModelLimit, 0, len(out.Overrides))
	for _, o := range out.Overrides {
		overrides = append(overrides, Normalize(o))
	}
	out.Overrides = overrides
	return out
}

func previewRationale(row Row, realm string) []string {
	limit := row
	out := []string{}
	if row.Source == SourceManual {
		out = append(out, "口径：手动设限（覆盖「跟随上游」）。")
	} else {
		out = append(out, "口径：跟随上游网关推算，本地不额外设预算；上游 6004 时按降级链改道。")
	}
	if row.Quota.Inferred {
		out = append(out, fmt.Sprintf("单账号 token 配额按上游推算：%s / %s（%s 域）。",
			formatTokens(row.Quota.TokenPerAccount), windowLabel(row.Quota.WindowSec), realm))
	} else {
		out = append(out, fmt.Sprintf("单账号 token 配额由策略指定：%s / %s。",
			formatTokens(row.Quota.TokenPerAccount), windowLabel(row.Quota.WindowSec)))
	}
	out = append(out, fmt.Sprintf("池级 token 配额 = 单账号配额 × %d 个可路由账号 = %s（已用 %s，%.0f%%）。",
		row.RoutableAccounts, formatTokens(row.Quota.TokenPerPool), formatTokens(row.Pool.UsedTokens), row.Pool.Pct*100))
	if row.Quota.CreditPerPool > 0 {
		out = append(out, fmt.Sprintf("池级积分预算：%s（已用 %s）。", formatCredits(row.Quota.CreditPerPool), formatCredits(row.Pool.UsedCredits)))
	}
	if len(row.Chain) == 0 {
		out = append(out, fmt.Sprintf("没有可用的降级链：额度耗尽时按 on_hit=%s 处理。", limit.OnHit))
		return out
	}
	names := make([]string, 0, len(row.Chain))
	for _, s := range row.Chain {
		label := s.Model
		if s.ZeroRate {
			label += "（零倍率）"
		}
		names = append(names, label)
	}
	out = append(out, fmt.Sprintf("降级链：%s（来源：%s）。", strings.Join(names, " → "), chainSourceLabel(row.ChainSource)))
	return out
}

// DecisionOf 把一行快照换算成请求路径的判定。
//
// 只有手动设限才在本地拦：跟随上游的行必须放过——上游额度才是权威，本地若也按
// 推算值拦，等于连着两层刹车，出问题时没人说得清该怪谁。
// degrade 但没有任何可用降级链时降级为 hold：宁可明确报错，也不让「设了限流却
// 照旧放行」把预算悄悄烧穿。
func DecisionOf(row Row) Decision {
	if row.Source != SourceManual || row.LimitState == "ok" {
		return Decision{}
	}
	hold := Decision{Action: ActionHold, Kind: limitKind(row), Reason: limitReason(row), RetryAfterSec: row.RetryAfterSec}
	switch row.OnHit {
	case OnHitPassthrough:
		return Decision{}
	case OnHitDegrade:
		if len(row.Chain) > 0 {
			return Decision{Action: ActionDegrade, Target: row.Chain[0].Model, Kind: limitKind(row), Reason: limitReason(row), RetryAfterSec: row.RetryAfterSec}
		}
		hold.Reason += "；且没有配置可用的降级链"
		return hold
	}
	return hold
}

// 判定动作。
const (
	ActionDegrade = "degrade"
	ActionHold    = "hold"
)

func limitKind(row Row) string {
	switch {
	case row.Pool.QuotaCredits > 0 && row.Pool.UsedCredits >= row.Pool.QuotaCredits:
		return "pool_credit"
	case row.Pool.QuotaTokens > 0 && row.Pool.UsedTokens >= row.Pool.QuotaTokens:
		return "tokens"
	}
	return "accounts"
}

func limitReason(row Row) string {
	switch limitKind(row) {
	case "pool_credit":
		return fmt.Sprintf("池级积分预算已用尽（%s / %s）", formatCredits(row.Pool.UsedCredits), formatCredits(row.Pool.QuotaCredits))
	case "tokens":
		return fmt.Sprintf("池级 token 额度已用尽（%s / %s，窗口 %s）",
			formatTokens(row.Pool.UsedTokens), formatTokens(row.Pool.QuotaTokens), windowLabel(row.WindowSec))
	}
	return fmt.Sprintf("该域已无账号接这个模型（%d/%d 个账号已限流）", row.LimitedAccounts, row.RoutableAccounts)
}

func collectRealms(d Deps, accounts []Account) []string {
	realms := []string{}
	if d.Realms != nil {
		realms = d.Realms()
	}
	if len(realms) == 0 {
		seen := map[string]bool{}
		for _, a := range accounts {
			r := normalizeRealm(a.Realm)
			if r != "" && !seen[r] {
				seen[r] = true
				realms = append(realms, r)
			}
		}
	}
	if len(realms) == 0 {
		realms = []string{"cn", "global"}
	}
	sort.SliceStable(realms, func(i, j int) bool { return realmOrder(realms[i]) < realmOrder(realms[j]) })
	return realms
}

// realmOrder 固定 cn < global < 其它（按名字）：面板主用前两个，顺序变了会让
// 打开模型限流映射页第一眼的行位置漂移。
func realmOrder(realm string) string {
	switch normalizeRealm(realm) {
	case "cn":
		return "0"
	case "global":
		return "1"
	}
	return "2" + normalizeRealm(realm)
}

// dropRule 报告某个 bare 名（已剥域前缀）是否要从页面与降级链里剔除。
type dropRule func(bare string) bool

// dropFor 组合两类剔除规则：运营手动拉黑的黑名单（livecfg.HiddenModels，可解除），
// 与客户端折叠出来的分类汇总别名（aliases.go，永远不该出现在页面上）。
// 黑名单为空时只留别名判定，省掉一次 map 查找。
func dropFor(hidden map[string]bool) dropRule {
	if len(hidden) == 0 {
		return isAggregateModel
	}
	return func(bare string) bool {
		return hidden[bare] || isAggregateModel(bare)
	}
}

// filterModels 按 drop 规则过滤模型名（支持带域前缀的名）。
func filterModels(models []string, realm string, drop dropRule) []string {
	if len(models) == 0 {
		return models
	}
	out := make([]string, 0, len(models))
	for _, name := range models {
		if drop(bareFor(name, realm)) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// filterRates 从倍率快照里剔除 drop 命中的模型：倍率表也是自动降级链的候选池，
// 留在里面会先占住链名额、再被 filterSteps 删掉，最终把付费兜底挤没
// （CN deepseek-v4.1-flash 的实际症状）。
func filterRates(rates map[string]string, realm string, drop dropRule) map[string]string {
	if len(rates) == 0 {
		return rates
	}
	out := make(map[string]string, len(rates))
	for name, rate := range rates {
		if drop(bareFor(name, realm)) {
			continue
		}
		out[name] = rate
	}
	return out
}

// filterSteps 剔除降级链里被 drop 命中的步骤（人工链可能写死了一个已拉黑/汇总的 id）。
func filterSteps(steps []Step, realm string, drop dropRule) []Step {
	if len(steps) == 0 {
		return steps
	}
	out := make([]Step, 0, len(steps))
	for _, s := range steps {
		if drop(bareFor(s.Model, realm)) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// adaptQuota 根据上游 6004 反馈收紧推算额度。上游突然缩减预算时，账号会在
// 窗口内撞 6004：此时把该域该模型的推算值下调到「已用 token + 一点余量」，
// 让页面显示的「可用额度」贴近上游真实口径，下一次推算周期继续跟随。
// 只收紧不放大：没有信号或信号弱时保持默认，避免误报「还有很多额度」。
func adaptQuota(q Quota, realm, model string, routable int, usage map[string]UsageRow, accounts []Account, now time.Time) Quota {
	if q.TokenPerAccount <= 0 {
		return q
	}
	var maxUsed int64
	for _, a := range accounts {
		u := usage[usageKey(realm, a.UID, model)]
		if u.Tokens > maxUsed {
			maxUsed = u.Tokens
		}
	}
	if maxUsed <= 0 {
		return q
	}
	// 有 6004 信号：把额度压到已用量附近（留 10% 窗口），但绝不低于已用量。
	signal := false
	for _, a := range accounts {
		if _, ok := accountRateLimit(a.RateLimited, model, now); ok {
			signal = true
			break
		}
	}
	if !signal {
		return q
	}
	adjusted := maxUsed + maxUsed/10
	if adjusted < q.TokenPerAccount {
		q.TokenPerAccount = adjusted
		q.TokenPerPool = q.TokenPerAccount * int64(routable)
	}
	return q
}

func collectModels(realm string, catalog []string, p Policy, usage map[string]UsageRow) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(catalog))
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, name)
	}
	for _, name := range catalog {
		add(name)
	}
	// 覆盖项里点名的模型即使当前不在目录里也要出现：运营刚配的策略不该因为目录
	// 还没刷新就凭空消失，否则「保存成功但看不到」是无法自证的。
	for _, o := range p.Overrides {
		n := Normalize(o)
		if n.Model == "" {
			continue
		}
		if n.Realm != "" && n.Realm != realm {
			continue
		}
		add(n.Model)
	}
	for _, row := range usage {
		if normalizeRealm(row.Realm) == realm {
			add(row.Model)
		}
	}
	sort.Strings(out)
	return out
}

// sortRows 行内排序：池级已用额度大的排最前（打开页面第一眼就是「谁在烧钱」），
// 已用相同再按剩余可用额度升序（快见底的更靠前），最后按域名/模型名稳定排序。
// 没有用量的行（已用 0）自然落到末尾，不需要为「无池级配额」单开一档。
func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Pool.UsedTokens != b.Pool.UsedTokens {
			return a.Pool.UsedTokens > b.Pool.UsedTokens
		}
		if a.Pool.RemainingTokens != b.Pool.RemainingTokens {
			return a.Pool.RemainingTokens < b.Pool.RemainingTokens
		}
		return a.FullName < b.FullName
	})
}

func limitState(routable, limitedRoutable int, pctTokens, pctCredits float64) string {
	switch {
	case routable == 0:
		return "locked"
	case limitedRoutable >= routable:
		return "locked"
	case limitedRoutable > 0 || pctTokens >= 0.9 || pctCredits >= 0.9:
		return "partial"
	}
	return "ok"
}

func usageKey(realm, uid, model string) string {
	return normalizeRealm(realm) + "/" + uid + "/" + strings.ToLower(strings.TrimSpace(model))
}

func accountRateLimit(limits []RateLimit, model string, now time.Time) (RateLimit, bool) {
	for _, rl := range limits {
		if !strings.EqualFold(rl.Model, model) {
			continue
		}
		if !rl.Until.IsZero() && rl.Until.Before(now) {
			continue // 已过期的冷却不算现行限流
		}
		return rl, true
	}
	return RateLimit{}, false
}

func pct(used, quota float64) float64 {
	if quota <= 0 {
		return 0
	}
	return used / quota
}

// formatTokens 把 token 数写成 M/B 口径（页面与说明文字统一用它）。
func formatTokens(v int64) string {
	switch {
	case v >= 1_000_000_000:
		return strconv.FormatFloat(float64(v)/1_000_000_000, 'f', 1, 64) + "B"
	case v >= 1_000_000:
		return strconv.FormatFloat(float64(v)/1_000_000, 'f', 1, 64) + "M"
	case v >= 1_000:
		return strconv.FormatFloat(float64(v)/1_000, 'f', 1, 64) + "K"
	}
	return strconv.FormatInt(v, 10)
}

func formatCredits(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64) + " 积分"
}

// windowLabel 把秒数写成「24h」这类人读形式。
func windowLabel(sec int64) string {
	if sec <= 0 {
		sec = DefaultWindowSec
	}
	if sec%3600 == 0 {
		return strconv.FormatInt(sec/3600, 10) + "h"
	}
	if sec%60 == 0 {
		return strconv.FormatInt(sec/60, 10) + "m"
	}
	return strconv.FormatInt(sec, 10) + "s"
}

func chainSourceLabel(source string) string {
	switch source {
	case ChainSourceManual:
		return "手动指定"
	case ChainSourceFallback:
		return "pool.model_fallback 表"
	case ChainSourceAutoZero:
		return "自动：零倍率优先"
	case ChainSourceAutoCheap:
		return "自动：最低倍率"
	}
	return "无"
}
