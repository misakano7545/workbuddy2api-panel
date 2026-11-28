package ratelimit

import (
	"testing"
	"time"
)

// 窗口边界这一组（quota_reset_at / retry_after_sec）是把「额度还剩多少」变成
// 「什么时候能再用」的落点。这三个数都建立在同一个近似上：用量只有整点桶粒度，
// 且上游窗口从该号该模型第一次请求起算（滚动 24h，不是自然日重置）。所以它们
// 只能回答「最早有多少额度会滚出窗口」，回答不了「额度到点就恢复」。

// windowDeps 是一个可路由号 + 一条零倍率降级目标，用量带窗口起点。
func windowDeps(now time.Time, usageTokens int64) Deps {
	return Deps{
		Now: func() time.Time { return now },
		Accounts: func() []Account {
			return []Account{{UID: "1001", Nickname: "alpha", Realm: "cn", Credits: 10}}
		},
		Realms: func() []string { return []string{"cn"} },
		Rates: func(realm string) map[string]string {
			return map[string]string{"m1": "1", "free-1": "0"}
		},
		Models:   func(realm string) []string { return []string{"m1", "free-1"} },
		Fallback: func() map[string]string { return nil },
		Usage: func(window time.Duration) []UsageRow {
			return []UsageRow{{
				Realm: "cn", UID: "1001", Model: "m1",
				Tokens: usageTokens, Requests: 3,
				FirstAt: now.Add(-23 * time.Hour),
			}}
		},
	}
}

// manualPolicy 是「手动设限 + 超额就降级到零倍率模型」的一份策略。
func manualPolicy() Policy {
	return Policy{
		Preset: "saver",
		Default: Normalize(ModelLimit{
			Source: SourceManual, TokenQuotaPerAccount: 100,
			ChainMode: ChainZero, OnHit: OnHitDegrade,
		}),
	}
}

// 已超额时：行上要给出窗口边界与建议重试秒数，判定里也要带着这个秒数，
// 否则请求路径拿不到 Retry-After 的依据。
func TestBuildQuotaResetAtAndRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	v := Build(manualPolicy(), windowDeps(now, 150))
	row := rowOf(t, v, "cn", "m1")

	if row.LimitState != "locked" {
		t.Fatalf("唯一可路由号已超额，状态应为 locked，得到 %q", row.LimitState)
	}
	wantReset := now.Add(-23 * time.Hour).Add(time.Duration(DefaultWindowSec) * time.Second).UTC().Format(time.RFC3339)
	if row.QuotaResetAt != wantReset {
		t.Fatalf("窗口边界应为最早用量 + 窗口长度 %s，得到 %q", wantReset, row.QuotaResetAt)
	}
	if row.RetryAfterSec != 3600 {
		t.Fatalf("建议重试秒数应为 3600（窗口边界距 now 一小时），得到 %d", row.RetryAfterSec)
	}
	d := DecisionOf(row)
	if d.Action != ActionDegrade {
		t.Fatalf("超额且有链时应改道，得到 %+v", d)
	}
	if d.RetryAfterSec != row.RetryAfterSec {
		t.Fatalf("判定里的重试秒数应与行一致：%d vs %d", d.RetryAfterSec, row.RetryAfterSec)
	}
	// 账号级也要有窗口边界：页面要能逐账号显示。
	if len(row.Accounts) != 1 || row.Accounts[0].QuotaResetAt != wantReset {
		t.Fatalf("账号级窗口边界应为 %s，得到 %+v", wantReset, row.Accounts)
	}
}

// 没吃紧就不给重试时刻：给了会让客户端以为要等，白白退避。
func TestBuildNoRetryAfterWhenHealthy(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	v := Build(manualPolicy(), windowDeps(now, 10))
	row := rowOf(t, v, "cn", "m1")
	if row.LimitState != "ok" {
		t.Fatalf("未超额时状态应为 ok，得到 %q", row.LimitState)
	}
	if row.RetryAfterSec != 0 {
		t.Fatalf("未吃紧时不该给重试秒数，得到 %d", row.RetryAfterSec)
	}
	if d := DecisionOf(row); d.Action != "" {
		t.Fatalf("未超额时不该有任何动作，得到 %+v", d)
	}
}

// 跟随上游口径的行永远不拦，也不给重试时刻（上游才是权威）。
func TestBuildFollowRowHasNoRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	v := Build(DefaultPolicy(), windowDeps(now, 150_000_000))
	row := rowOf(t, v, "cn", "m1")
	if row.Source != SourceFollow {
		t.Fatalf("出厂口径应是跟随上游，得到 %q", row.Source)
	}
	if row.RetryAfterSec != 0 {
		t.Fatalf("跟随口径不该给重试秒数，得到 %d", row.RetryAfterSec)
	}
}

// 上游限流原因要透传到账号明细：页面只显示「已限流」看不出是配额耗尽还是别的。
func TestBuildCarriesUpstreamLimitReason(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	d := windowDeps(now, 10)
	base := d.Accounts
	d.Accounts = func() []Account {
		list := base()
		list[0].RateLimited = []RateLimit{{
			Model: "m1", Kind: "6004",
			Reason:  "model quota exhausted for this account",
			Until:   now.Add(10 * time.Minute),
			ResetAt: now.Add(10 * time.Minute),
		}}
		return list
	}
	v := Build(manualPolicy(), d)
	row := rowOf(t, v, "cn", "m1")
	if len(row.Accounts) != 1 {
		t.Fatalf("应有 1 个账号明细，得到 %d", len(row.Accounts))
	}
	ar := row.Accounts[0]
	if !ar.Limited || ar.LimitKind != "6004" {
		t.Fatalf("账号应被标成上游限流(6004)，得到 %+v", ar)
	}
	if ar.Reason != "model quota exhausted for this account" {
		t.Fatalf("限流原因应透传，得到 %q", ar.Reason)
	}
}
