package ratelimit

import (
	"strings"
	"testing"
	"time"
)

func TestHiddenModelsAreExcluded(t *testing.T) {
	d := testDeps(time.Now())
	d.Models = func(realm string) []string {
		if realm == "cn" {
			return []string{"m1", "free-1", "secret-1"}
		}
		return []string{"m1"}
	}
	d.Usage = func(time.Duration) []UsageRow {
		return []UsageRow{
			{Realm: "cn", UID: "1001", Model: "m1", Tokens: 100},
			{Realm: "cn", UID: "1001", Model: "secret-1", Tokens: 50},
			{Realm: "cn", UID: "1001", Model: "hidden-by-policy", Tokens: 1},
		}
	}
	d.Hidden = func(realm string) map[string]bool {
		if realm == "cn" {
			return map[string]bool{"secret-1": true, "hidden-by-policy": true, "free-1": true}
		}
		return nil
	}
	p := Policy{
		Default: Normalize(ModelLimit{Source: SourceFollow}),
		Overrides: []ModelLimit{
			Normalize(ModelLimit{Realm: "cn", Model: "hidden-by-policy", Source: SourceManual, TokenQuotaPerAccount: 10}),
		},
	}
	v := Build(p, d)
	for _, row := range v.Rows {
		if row.Realm == "cn" && (row.Model == "secret-1" || row.Model == "hidden-by-policy" || row.Model == "free-1") {
			t.Errorf("黑名单模型 %s 不该出现在快照里", row.FullName)
		}
	}
	if _, ok := findRow(v, "cn", "m1"); !ok {
		t.Fatal("正常模型不该被误隐藏")
	}
	p2 := Policy{Default: Normalize(ModelLimit{Source: SourceManual, ChainMode: ChainAuto})}
	v2 := Build(p2, d)
	if row, ok := findRow(v2, "cn", "m1"); ok {
		for _, s := range row.Chain {
			if s.Model == "free-1" {
				t.Errorf("降级链不该落到黑名单模型 free-1：%+v", row.Chain)
			}
		}
	}
	path := t.TempDir() + "/model_limits.json"
	tr := NewTracker(TrackerConfig{Path: path, Deps: d})
	if got := tr.Decide("cn", "secret-1"); got.Action != "" {
		t.Errorf("隐藏模型的决策必须为零值，得到 %+v", got)
	}
	if got := tr.NextChainTarget("cn", "m1", nil); got == "free-1" {
		t.Errorf("隐藏模型不该出现在链缓存里，得到 %q", got)
	}
}

// 隐藏模型即便还留在上游倍率快照里，也不能占自动链的名额：CN 实测有 4 个零倍率
// 候选，其中 1 个被隐藏；若先按 4 个零倍率挑满再过滤，付费兜底就被挤掉了。
func TestHiddenModelsDoNotConsumeChainSlots(t *testing.T) {
	d := testDeps(time.Now())
	d.Rates = func(realm string) map[string]string {
		if realm == "cn" {
			return map[string]string{"m1": "1", "a-hidden": "0", "free-b": "0", "free-c": "0", "free-d": "0", "free-e": "0", "paid": "0.5"}
		}
		return nil
	}
	d.Models = func(realm string) []string {
		if realm == "cn" {
			return []string{"m1", "a-hidden", "free-b", "free-c", "free-d", "free-e", "paid"}
		}
		return nil
	}
	d.Hidden = func(realm string) map[string]bool {
		if realm == "cn" {
			return map[string]bool{"a-hidden": true}
		}
		return nil
	}
	p := Policy{Default: Normalize(ModelLimit{Source: SourceManual, ChainMode: ChainAuto})}
	row := rowOf(t, Build(p, d), "cn", "m1")
	want := "free-b,free-c,free-d,paid"
	got := make([]string, 0, len(row.Chain))
	for _, s := range row.Chain {
		got = append(got, s.Model)
	}
	if strings.Join(got, ",") != want {
		t.Fatalf("隐藏的零倍率不该占链名额、也不该挤掉付费兜底：got %s want %s", strings.Join(got, ","), want)
	}
}

// 行序：池级已用额度降序优先（一眼看到谁在烧钱），已用相同按剩余升序，
// 没用量/没配额的行自然落到末尾 —— 与前端 mrlSortRows 同一口径。
func TestSortRowsByUsedTokensDescending(t *testing.T) {
	rows := []Row{
		{Model: "noquota", FullName: "cn:noquota"},
		{Model: "small", FullName: "cn:small", Pool: PoolUsage{QuotaTokens: 200_000_000, UsedTokens: 400_000_000}},
		{Model: "wide-b", FullName: "cn:wide-b", Pool: PoolUsage{QuotaTokens: 1_200_000_000, UsedTokens: 900_000_000, RemainingTokens: 300_000_000}},
		{Model: "mid", FullName: "cn:mid", Pool: PoolUsage{QuotaTokens: 600_000_000, UsedTokens: 900_000_000}},
		{Model: "over", FullName: "cn:over", Pool: PoolUsage{QuotaTokens: 600_000_000, UsedTokens: 941_900_000}},
	}
	sortRows(rows)
	var order []string
	for _, r := range rows {
		order = append(order, r.Model)
	}
	// mid 与 wide-b 已用同为 900M，剩余少的 mid 先；配额大小不参与排序（small 只有 200M
	// 配额但用了 400M，排在配额 1.2B 的 wide-b 前面）。
	want := "over,mid,wide-b,small,noquota"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("行应按池级已用额度降序、同已用按剩余升序：got %q want %q", got, want)
	}
}

func TestResolveQuotaWithSignals(t *testing.T) {
	d := testDeps(time.Now())
	d.Usage = func(time.Duration) []UsageRow {
		return []UsageRow{
			{Realm: "cn", UID: "1001", Model: "m1", Tokens: 150_000_000},
		}
	}
	d.Accounts = func() []Account {
		return []Account{{
			UID: "1001", Realm: "cn", Credits: 10,
			RateLimited: []RateLimit{{Model: "m1", Kind: "rate_limit", Reason: "code 6004: quota exhausted"}},
		}}
	}
	v := Build(DefaultPolicy(), d)
	row := rowOf(t, v, "cn", "m1")
	if row.Quota.TokenPerAccount >= 200_000_000 {
		t.Errorf("6004 命中后推算额度应低于默认 200M，得到 %d", row.Quota.TokenPerAccount)
	}
	if row.Quota.TokenPerAccount < 150_000_000 {
		t.Errorf("推算额度不该低于已用 token，得到 %d", row.Quota.TokenPerAccount)
	}
	if !row.Quota.Inferred {
		t.Error("自适应推算仍应标记 inferred，不能伪装成合同值")
	}
	p := Policy{Default: Normalize(ModelLimit{Source: SourceManual, TokenQuotaPerAccount: 100_000_000})}
	manual := Build(p, d)
	if got := rowOf(t, manual, "cn", "m1").Quota.TokenPerAccount; got != 100_000_000 {
		t.Errorf("手动配额不该被信号改写，得到 %d", got)
	}
}

func findRow(v View, realm, model string) (Row, bool) {
	for _, r := range v.Rows {
		if r.Realm == realm && r.Model == model {
			return r, true
		}
	}
	return Row{}, false
}

// 客户端的「分类汇总」别名（deep-model / balanced-model 这类）不是可调用模型：
// 整类从页面剔除 —— 目录里有、历史用量里有、人工链里写死、倍率表里报零倍率，
// 四种入口都不能漏。
func TestAggregateAliasesAreExcluded(t *testing.T) {
	aliases := []string{"deep-model", "balanced-model", "fast-model", "primary-model", "default-model", "auto", "auto-chat"}
	d := testDeps(time.Now())
	d.Models = func(realm string) []string {
		if realm == "cn" {
			return append([]string{"m1", "free-1"}, aliases...)
		}
		return []string{"m1"}
	}
	d.Rates = func(realm string) map[string]string {
		if realm != "cn" {
			return map[string]string{"m1": "0.5"}
		}
		rates := map[string]string{"m1": "1", "free-1": "0"}
		for _, a := range aliases {
			rates[a] = "0" // 别名就算报了零倍率，也不该占链名额
		}
		return rates
	}
	d.Usage = func(time.Duration) []UsageRow {
		return []UsageRow{
			{Realm: "cn", UID: "1001", Model: "m1", Tokens: 100},
			{Realm: "cn", UID: "1001", Model: "balanced-model", Tokens: 50},
		}
	}
	p := Policy{Overrides: []ModelLimit{
		Normalize(ModelLimit{Realm: "cn", Model: "m1", Source: SourceManual, Chain: []string{"deep-model", "free-1"}}),
	}}
	v := Build(p, d)
	for _, row := range v.Rows {
		if isAggregateModel(row.Model) {
			t.Errorf("分类汇总别名不该出现在快照里：%s", row.FullName)
		}
	}
	if _, ok := findRow(v, "cn", "free-1"); !ok {
		t.Error("正常模型不该被误剔除")
	}
	row := rowOf(t, v, "cn", "m1")
	if len(row.Chain) != 1 || row.Chain[0].Model != "free-1" {
		t.Errorf("人工链里写死的别名应被剔除，只剩 free-1：%+v", row.Chain)
	}

	// 自动链：别名不能占住名额（否则同 hidden，付费兜底会被挤掉）。
	p2 := Policy{Default: Normalize(ModelLimit{Source: SourceManual, ChainMode: ChainZero})}
	v2 := Build(p2, d)
	row2 := rowOf(t, v2, "cn", "m1")
	if len(row2.Chain) == 0 {
		t.Fatal("自动链不该被别名挤空")
	}
	for _, s := range row2.Chain {
		if isAggregateModel(s.Model) {
			t.Errorf("降级链不该落到分类汇总别名：%+v", row2.Chain)
		}
	}
	if got := row2.Chain[0].Model; got != "free-1" {
		t.Errorf("零倍率自动链应落到 free-1，得到 %q（%+v）", got, row2.Chain)
	}
}
