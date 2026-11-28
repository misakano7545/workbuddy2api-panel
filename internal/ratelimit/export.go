package ratelimit

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// BundleVersion 是导出包格式版本：上游侧按版本号决定能不能直接消费。
const BundleVersion = 1

// Bundle 是「提交给上游」的策略包：一份能被人读、也能被机器消费的完整口径。
//
// 为什么导出包要同时给人读（Markdown）与给机器读（JSON 字段）：提上游时两件事
// 都得说清——「这个功能想表达什么口径」（人读，用于 issue/PR 讨论）与「按你现在的
// 配置该算出什么数」（机器读，用于上游复算与对齐）。只有 JSON 会让人在评审里
// 重新猜口径，只有 Markdown 则没法复算。
type Bundle struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	// Basis 说明额度是怎么来的（上游推算 vs 手动），提上游时必须随包附上：
	// 上游评审最先问的就是「200M 这个数你是从哪知道的」。
	Basis []string `json:"basis"`

	Policy Policy `json:"policy"`

	Limits []LimitEntry `json:"limits"`
	Chains []ChainEntry `json:"chains"`

	// FallbackSuggestion 是按当前链解析结果反推出的 pool.model_fallback 表：
	// 上游若只想用老机制（仅 6004 改道、单级），可以直接把这张表填进配置。
	FallbackSuggestion map[string]string `json:"model_fallback_suggestion,omitempty"`
	// ConfigExcerpt 是建议的上游配置片段（说明性结构，不直接回写配置）。
	ConfigExcerpt map[string]any `json:"config_excerpt,omitempty"`

	Notes    []string `json:"notes,omitempty"`
	Markdown string   `json:"markdown"`
}

// LimitEntry 是扁平化的生效口径（导出包与上游对齐时逐行核对用）。
type LimitEntry struct {
	Realm           string  `json:"realm"`
	Model           string  `json:"model"`
	Rate            string  `json:"rate,omitempty"`
	Source          string  `json:"source"`
	OnHit           string  `json:"on_hit"`
	ChainMode       string  `json:"chain_mode"`
	WindowSec       int64   `json:"window_sec"`
	TokenPerAccount int64   `json:"token_per_account"`
	CreditPerPool   float64 `json:"credit_per_pool,omitempty"`
	QuotaInferred   bool    `json:"quota_inferred"`
	PoolQuotaTokens int64   `json:"pool_quota_tokens"`
	UsedTokens      int64   `json:"used_tokens"`
	Pct             float64 `json:"pct"`
	LimitState      string  `json:"limit_state"`
}

// ChainEntry 是某 (域, 模型) 的降级链。
type ChainEntry struct {
	Realm  string `json:"realm"`
	Model  string `json:"model"`
	Source string `json:"source"`
	Steps  []Step `json:"steps"`
}

// BuildBundle 把当前策略与快照打包成提交包。
func BuildBundle(p Policy, v View) Bundle {
	b := Bundle{
		Version:     BundleVersion,
		GeneratedAt: time.Now().UTC(),
		Policy:      p,
		Basis: []string{
			fmt.Sprintf("跟随上游口径：每账号每模型每 %s 窗口 CN %s token / global %s token（滚动窗口，从该账号该模型窗口内第一次请求起算，不是自然日重置）。",
				windowLabel(DefaultWindowSec), formatTokens(FollowTokenQuota("cn")), formatTokens(FollowTokenQuota("global"))),
			"池级配额 = 单账号配额 × 该域参与选号的账号数（禁用/暂停号不计入）。",
			"消耗取自面板用量记录器的小时桶（tokens / credits），窗口内累加，并按**本轮实际出站的模型**归属：6004 降级到等价模型后记在等价模型名下，不混算到请求原模型上（混算会让原模型虚高、出现「已用 > 池级配额」）。",
			"上游 6004 返回的恢复时刻写入账号的模型级冷却，页面按账号展示。",
		},
		FallbackSuggestion: map[string]string{},
	}
	for _, row := range v.Rows {
		b.Limits = append(b.Limits, LimitEntry{
			Realm: row.Realm, Model: row.Model, Rate: row.Rate,
			Source: row.Source, OnHit: row.OnHit, ChainMode: row.ChainMode,
			WindowSec: row.WindowSec, TokenPerAccount: row.Quota.TokenPerAccount,
			CreditPerPool: row.Quota.CreditPerPool, QuotaInferred: row.Quota.Inferred,
			PoolQuotaTokens: row.Pool.QuotaTokens, UsedTokens: row.Pool.UsedTokens,
			Pct: row.Pool.Pct, LimitState: row.LimitState,
		})
		if len(row.Chain) == 0 {
			continue
		}
		b.Chains = append(b.Chains, ChainEntry{Realm: row.Realm, Model: row.Model, Source: row.ChainSource, Steps: row.Chain})
		// 只给「单级链」写建议：多级链老机制表达不了，硬塞进 model_fallback
		// 会得到一个「看起来等价、实际只降一级」的假配置。
		if len(row.Chain) == 1 && len(b.FallbackSuggestion) < 64 {
			if _, exists := b.FallbackSuggestion[row.Model]; !exists {
				b.FallbackSuggestion[row.Model] = row.Chain[0].Model
			}
		}
	}
	if len(b.FallbackSuggestion) > 0 {
		b.ConfigExcerpt = map[string]any{
			"pool": map[string]any{"model_fallback": b.FallbackSuggestion},
		}
	}
	sort.SliceStable(b.Limits, func(i, j int) bool {
		if b.Limits[i].Realm != b.Limits[j].Realm {
			return realmOrder(b.Limits[i].Realm) < realmOrder(b.Limits[j].Realm)
		}
		return b.Limits[i].Model < b.Limits[j].Model
	})
	b.Notes = bundleNotes(p, v)
	b.Markdown = bundleMarkdown(b, v)
	return b
}

func bundleNotes(p Policy, v View) []string {
	notes := []string{}
	if p.Preset != "" {
		if preset, ok := FindPreset(p.Preset); ok {
			notes = append(notes, "当前预设："+preset.Name+"——"+preset.Note)
		}
	}
	if p.Default.Source == SourceManual {
		notes = append(notes, "默认口径为手动设限：额度耗尽会在本地拦/改道，不依赖上游 6004 反馈。")
	} else {
		notes = append(notes, "默认口径为跟随上游：本地只观测，真正撞顶仍由上游 6004 触发改道。")
	}
	if len(p.Overrides) > 0 {
		notes = append(notes, fmt.Sprintf("含 %d 条覆盖项（按域/模型细化），越具体越优先。", len(p.Overrides)))
	}
	if v.Totals.Limited > 0 || v.Totals.Degrading > 0 {
		notes = append(notes, fmt.Sprintf("导出时 %d 个模型 locked（该域已无账号可接）、%d 个正在降级。",
			v.Totals.Limited, v.Totals.Degrading))
	}
	return notes
}

// bundleMarkdown 生成随包附带的口径说明。刻意写成「评审可直接引用的短段落」，
// 不铺陈实现细节——上游要看的是口径与边界，不是这个 fork 的分支名。
func bundleMarkdown(b Bundle, v View) string {
	var sb strings.Builder
	sb.WriteString("## 模型限流映射策略包\n\n")
	sb.WriteString(fmt.Sprintf("生成时间：%s（策略更新时间：%s）\n\n",
		b.GeneratedAt.Local().Format("2006-01-02 15:04:05 MST"), formatTime(b.Policy.UpdatedAt)))

	sb.WriteString("### 口径依据\n\n")
	for _, line := range b.Basis {
		sb.WriteString("- " + line + "\n")
	}
	sb.WriteString("\n### 生效额度（按域）\n\n")
	sb.WriteString("| 域 | 模型 | 倍率 | 口径 | 单账号窗口配额 | 池级配额 | 已用 | 状态 |\n")
	sb.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, l := range b.Limits {
		rate := l.Rate
		if rate == "" {
			rate = "未知"
		}
		quotaLabel := formatTokens(l.TokenPerAccount)
		if l.QuotaInferred {
			quotaLabel += "（推算）"
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s（%.0f%%） | %s |\n",
			l.Realm, l.Model, rate, sourceLabel(l.Source), quotaLabel,
			formatTokens(l.PoolQuotaTokens), formatTokens(l.UsedTokens), l.Pct*100, stateLabel(l.LimitState)))
	}

	if len(b.Chains) > 0 {
		sb.WriteString("\n### 降级链\n\n")
		for _, c := range b.Chains {
			names := make([]string, 0, len(c.Steps))
			for _, s := range c.Steps {
				label := s.Model
				if s.ZeroRate {
					label += "(0)"
				}
				names = append(names, label)
			}
			sb.WriteString(fmt.Sprintf("- %s:%s → %s（来源：%s）\n", c.Realm, c.Model, strings.Join(names, " → "), chainSourceLabel(c.Source)))
		}
	}

	if len(b.FallbackSuggestion) > 0 {
		sb.WriteString("\n### 若只用现有机制（pool.model_fallback）\n\n")
		sb.WriteString("```yaml\npool:\n  model_fallback:\n")
		keys := make([]string, 0, len(b.FallbackSuggestion))
		for k := range b.FallbackSuggestion {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("    %s: %s\n", k, b.FallbackSuggestion[k]))
		}
		sb.WriteString("```\n")
		sb.WriteString("\n注意：该表只能表达单级改道且仅在上游 6004 后生效，无法表达本地预算拦截、多级链与按积分维度限流。\n")
	}

	if len(b.Notes) > 0 {
		sb.WriteString("\n### 备注\n\n")
		for _, n := range b.Notes {
			sb.WriteString("- " + n + "\n")
		}
	}

	sb.WriteString("\n### 现状摘要\n\n")
	sb.WriteString(fmt.Sprintf("%d 个模型 / %d 个账号（%d 可路由）/ 剩余积分 %.0f / 窗口消耗 %s tokens（%d 次请求）\n",
		v.Totals.Models, v.Totals.Accounts, v.Totals.Routable, v.Totals.Credits,
		formatTokens(v.Totals.Tokens), v.Totals.Requests))
	return sb.String()
}

func sourceLabel(source string) string {
	if source == SourceManual {
		return "手动"
	}
	return "跟随上游"
}

func stateLabel(state string) string {
	switch state {
	case "locked":
		return "已锁（无可用账号）"
	case "partial":
		return "吃紧"
	}
	return "正常"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "未保存过（出厂默认）"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
