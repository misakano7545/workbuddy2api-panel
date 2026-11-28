package ratelimit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testDeps 是一份最小但真实的注入集：两个 cn 号（其中一个被禁用，不能算进池级
// 额度）+ 一个 global 号，cn 有一个零倍率模型当降级目标。
func testDeps(now time.Time) Deps {
	return Deps{
		Now: func() time.Time { return now },
		Accounts: func() []Account {
			return []Account{
				{UID: "1001", Nickname: "alpha", Realm: "cn", Credits: 10},
				{UID: "1002", Realm: "cn", Credits: 5, Disabled: true},
				{UID: "2001", Nickname: "beta", Realm: "global", Credits: 7},
			}
		},
		Realms: func() []string { return []string{"cn", "global"} },
		Rates: func(realm string) map[string]string {
			if realm == "cn" {
				return map[string]string{"m1": "1", "free-1": "0"}
			}
			return map[string]string{"m1": "0.5"}
		},
		Models: func(realm string) []string {
			if realm == "cn" {
				return []string{"m1", "free-1"}
			}
			return []string{"m1"}
		},
		Fallback: func() map[string]string { return nil },
		Usage: func(window time.Duration) []UsageRow {
			return []UsageRow{
				{Realm: "cn", UID: "1001", Model: "m1", Tokens: 100, Requests: 2, Credits: 0.5},
				{Realm: "cn", UID: "1002", Model: "m1", Tokens: 40, Requests: 1},
			}
		},
	}
}

func rowOf(t *testing.T, v View, realm, model string) Row {
	t.Helper()
	for _, r := range v.Rows {
		if r.Realm == realm && r.Model == model {
			return r
		}
	}
	t.Fatalf("快照里找不到 %s:%s（现有 %d 行）", realm, model, len(v.Rows))
	return Row{}
}

func TestBuildFollowOnlyObserves(t *testing.T) {
	p := DefaultPolicy()
	v := Build(p, testDeps(time.Now()))
	row := rowOf(t, v, "cn", "m1")

	if row.Source != SourceFollow {
		t.Errorf("出厂口径应是跟随上游，得到 %q", row.Source)
	}
	if !row.Quota.Inferred || row.Quota.TokenPerAccount != 200_000_000 {
		t.Errorf("零配额应回落到上游推算的 200M，得到 %+v", row.Quota)
	}
	// 被禁用的 1002 不参与选号，池级额度只按 1 个号算。
	if row.RoutableAccounts != 1 || row.Quota.TokenPerPool != 200_000_000 {
		t.Errorf("池级额度应只按可路由账号算，得到 routable=%d pool=%d", row.RoutableAccounts, row.Quota.TokenPerPool)
	}
	if row.Usage.Tokens != 140 || row.Pool.UsedTokens != 100 {
		t.Errorf("用量：该模型总 token 应含禁用号（140），池级只算可路由号（100），得到 %+v / %+v", row.Usage, row.Pool)
	}
	if row.LimitState != "ok" {
		t.Errorf("跟随口径下状态应为 ok，得到 %q", row.LimitState)
	}
	if got := DecisionOf(row); got.Action != "" {
		t.Errorf("跟随上游的行绝不能在本地拦，得到 %+v", got)
	}
	// 汇总：cn 两个号（其中一个禁用）
	if s := v.Summary["cn"]; s.Accounts != 2 || s.Routable != 1 || s.Credits != 15 {
		t.Errorf("cn 域摘要不对：%+v", s)
	}
}

func TestBuildManualEnforces(t *testing.T) {
	p := Policy{
		Preset: "saver",
		Default: Normalize(ModelLimit{
			Source: SourceManual, TokenQuotaPerAccount: 100,
			ChainMode: ChainZero, OnHit: OnHitDegrade,
		}),
	}
	v := Build(p, testDeps(time.Now()))
	row := rowOf(t, v, "cn", "m1")

	// 1001 已用 100 = 单号配额 -> 这个号在该模型上不再接单；而它是唯一可路由号。
	if row.LimitedAccounts != 1 || row.LimitState != "locked" {
		t.Fatalf("额度用尽应把该行标成 locked，得到 limited=%d state=%q", row.LimitedAccounts, row.LimitState)
	}
	if row.Pool.QuotaTokens != 100 || row.Pool.UsedTokens != 100 || row.Pool.Pct != 1 {
		t.Errorf("池级进度应满，得到 %+v", row.Pool)
	}
	if len(row.Chain) != 1 || row.Chain[0].Model != "free-1" || !row.Chain[0].ZeroRate {
		t.Fatalf("省钱口径的自动链应指向零倍率模型，得到 %+v", row.Chain)
	}
	if row.ChainSource != ChainSourceAutoZero {
		t.Errorf("链来源应为 auto-zero，得到 %q", row.ChainSource)
	}
	d := DecisionOf(row)
	if d.Action != ActionDegrade || d.Target != "free-1" {
		t.Fatalf("手动口径超限且有零倍率目标时应改道，得到 %+v", d)
	}
	// 池级额度等于唯一可路由号的额度，所以此刻池级也正好打满：先说清是哪个维度
	// 命中，运维才知道该调账号配额还是池级预算。
	if d.Kind != "tokens" {
		t.Errorf("池级 token 额度已打满，命中原因应为 tokens，得到 %q（%s）", d.Kind, d.Reason)
	}
	if !strings.Contains(d.Reason, "100") {
		t.Errorf("命中原因应带上真实数字，得到 %q", d.Reason)
	}
}

func TestDecisionPassthroughAndHold(t *testing.T) {
	base := Row{Realm: "cn", Model: "m1", Source: SourceManual, OnHit: OnHitPassthrough,
		LimitState: "locked", Pool: PoolUsage{QuotaTokens: 10, UsedTokens: 10}}
	if got := DecisionOf(base); got.Action != "" {
		t.Errorf("passthrough 只记录不拦，得到 %+v", got)
	}

	hold := base
	hold.OnHit = OnHitHold
	if got := DecisionOf(hold); got.Action != ActionHold {
		t.Errorf("hold 应直接拦，得到 %+v", got)
	}

	degradeNoChain := base
	degradeNoChain.OnHit = OnHitDegrade
	if got := DecisionOf(degradeNoChain); got.Action != ActionHold {
		t.Errorf("想降级却没有链时应退化为拦，得到 %+v", got)
	}
	if !strings.Contains(DecisionOf(degradeNoChain).Reason, "没有配置可用的降级链") {
		t.Errorf("退化原因应写进 reason，得到 %q", DecisionOf(degradeNoChain).Reason)
	}

	follow := base
	follow.Source = SourceFollow
	if got := DecisionOf(follow); got.Action != "" {
		t.Errorf("跟随上游的行不能因为本地算出的状态就拦，得到 %+v", got)
	}
}

func TestBuildCreditQuota(t *testing.T) {
	p := Policy{Default: Normalize(ModelLimit{
		Source: SourceManual, CreditQuotaPerAccount: 0.1, OnHit: OnHitHold,
	})}
	v := Build(p, testDeps(time.Now()))
	row := rowOf(t, v, "cn", "m1")
	// 1001 花了 0.5 积分，单号预算 0.1，池级 = 0.1 x 1 = 0.1 -> 积分维度已打满。
	if row.Pool.QuotaCredits != 0.1 || row.Pool.UsedCredits != 0.5 {
		t.Fatalf("池级积分口径不对：%+v", row.Pool)
	}
	// 账号在这个模型上并没有被上游限流，打满的是本地预算，所以状态是「吃紧」
	// 而不是「已锁」——状态标签描述账号可用性，预算维度用命中原因单独说明。
	if row.LimitState != "partial" {
		t.Errorf("预算打满应标 partial（账号仍可接），得到 %q", row.LimitState)
	}
	if d := DecisionOf(row); d.Action != ActionHold || d.Kind != "pool_credit" {
		t.Errorf("积分维度命中应拦且原因是 pool_credit，得到 %+v", d)
	}
}

func TestPreviewRationale(t *testing.T) {
	p := Policy{Default: Normalize(ModelLimit{Source: SourceManual, TokenQuotaPerAccount: 1000, ChainMode: ChainZero})}
	pre := PreviewWith(p, testDeps(time.Now()), "cn", "m1")
	if pre.Realm != "cn" || pre.Model != "m1" {
		t.Fatalf("预览对象不对：%+v", pre)
	}
	if len(pre.Rationale) == 0 {
		t.Fatal("预览必须说明每个数字怎么来的")
	}
	joined := strings.Join(pre.Rationale, " ")
	if !strings.Contains(joined, "1") {
		t.Errorf("reasonale 应包含可路由号数，得到 %q", joined)
	}
	if len(pre.Chain) != 1 || pre.Chain[0].Model != "free-1" {
		t.Errorf("预览应给出推算的链，得到 %+v", pre.Chain)
	}
}

func TestBuildBundle(t *testing.T) {
	p := Policy{Preset: "saver", Default: Normalize(ModelLimit{
		Source: SourceManual, TokenQuotaPerAccount: 100, ChainMode: ChainZero, OnHit: OnHitDegrade,
	})}
	v := Build(p, testDeps(time.Now()))
	b := BuildBundle(p, v)

	if b.Version != BundleVersion {
		t.Errorf("导出包版本应为 %d，得到 %d", BundleVersion, b.Version)
	}
	if len(b.Limits) != len(v.Rows) {
		t.Errorf("逐行口径应覆盖全部行：%d != %d", len(b.Limits), len(v.Rows))
	}
	if len(b.Basis) == 0 || !strings.Contains(b.Markdown, "口径依据") {
		t.Fatal("导出包必须写清额度口径依据（上游评审第一个问题）")
	}
	if !strings.Contains(b.Markdown, "### 降级链") {
		t.Error("有链时 markdown 应列出降级链")
	}
	// 单级链才反推 pool.model_fallback 建议
	if got := b.FallbackSuggestion["m1"]; got != "free-1" {
		t.Errorf("单级链应反推出 model_fallback 建议，得到 %q", got)
	}
	if b.ConfigExcerpt == nil {
		t.Error("有建议时应给出配置片段")
	}
}

func TestBundleSkipsMultiStepFallbackSuggestion(t *testing.T) {
	p := Policy{Default: Normalize(ModelLimit{
		Source: SourceManual, TokenQuotaPerAccount: 10, Chain: []string{"a", "b"},
	})}
	v := View{Rows: []Row{{
		Realm: "cn", Model: "m1", Source: SourceManual, ChainMode: ChainOff,
		Chain:       []Step{{Model: "a"}, {Model: "b"}},
		ChainSource: ChainSourceManual, LimitState: "locked",
	}}}
	b := BuildBundle(p, v)
	if _, ok := b.FallbackSuggestion["m1"]; ok {
		t.Error("多级链无法用 model_fallback 表达，不该给假等价的单级建议")
	}
	if len(b.Chains) != 1 || len(b.Chains[0].Steps) != 2 {
		t.Errorf("多级链仍应完整进导出包，得到 %+v", b.Chains)
	}
}

func TestTrackerPersistenceAndDecide(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model_limits.json")
	deps := testDeps(time.Now())

	tr := NewTracker(TrackerConfig{Path: path, Deps: deps})
	if tr.Policy().Preset != "follow" {
		t.Fatalf("全新部署应给出跟随上游的默认口径，得到 %q", tr.Policy().Preset)
	}
	if got := tr.Decide("cn", "m1"); got.Action != "" {
		t.Fatalf("默认口径下请求路径不该有任何动作，得到 %+v", got)
	}

	if _, err := tr.Upsert(ModelLimit{
		Realm: "cn", Model: "m1", Source: SourceManual,
		TokenQuotaPerAccount: 100, ChainMode: ChainZero, OnHit: OnHitDegrade,
	}); err != nil {
		t.Fatalf("写入覆盖项失败：%v", err)
	}
	d := tr.Decide("cn", "m1")
	if d.Action != ActionDegrade || d.Target != "free-1" {
		t.Fatalf("写入后应立刻生效（页面与请求路径用同一份快照），得到 %+v", d)
	}
	if got := tr.NextChainTarget("cn", "m1", map[string]bool{"free-1": true}); got != "" {
		t.Errorf("已试过的目标应跳过，得到 %q", got)
	}
	if got := tr.NextChainTarget("cn", "m1", nil); got != "free-1" {
		t.Errorf("未试过时应返回链首，得到 %q", got)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("策略应落盘：%v", err)
	}
	reloaded := NewTracker(TrackerConfig{Path: path, Deps: deps})
	if got := len(reloaded.Policy().Overrides); got != 1 {
		t.Fatalf("重启后应读回 1 条覆盖项，得到 %d", got)
	}
	if got := reloaded.Policy().Overrides[0].TokenQuotaPerAccount; got != 100 {
		t.Errorf("重启后额度应保留，得到 %d", got)
	}

	// 套预设会整体改写策略
	if _, err := reloaded.ApplyPreset("quality"); err != nil {
		t.Fatalf("套用预设失败：%v", err)
	}
	if got := reloaded.Policy().Default.OnHit; got != OnHitHold {
		t.Errorf("质量预设应改成直接拦，得到 %q", got)
	}
	// 回到出厂口径
	if _, err := reloaded.Reset(); err != nil {
		t.Fatalf("重置失败：%v", err)
	}
	if got := reloaded.Decide("cn", "m1"); got.Action != "" {
		t.Errorf("重置后应恢复放行，得到 %+v", got)
	}
}

func TestTrackerDisabledDoesNotBlock(t *testing.T) {
	var tr *Tracker
	if got := tr.Decide("cn", "m1"); got.Action != "" {
		t.Errorf("未启用时决策必须为零值（加性能力不能拦请求），得到 %+v", got)
	}
	if got := tr.View(); len(got.Presets) == 0 {
		t.Error("未启用时页面仍应拿到预设列表")
	}
	if _, err := tr.Save(DefaultPolicy()); err == nil {
		t.Error("未启用时保存应报错，而不是静默丢弃")
	}
}

func TestLoadPolicyBrokenFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(badPath)
	if err == nil {
		t.Error("坏文件应报错（要进日志）")
	}
	if p.Preset != "follow" {
		t.Errorf("坏文件应回落到出厂口径，不能把请求路径带崩，得到 %q", p.Preset)
	}

	// 版本高于本程序：同样回落并报错
	futurePath := filepath.Join(dir, "future.json")
	if err := os.WriteFile(futurePath, []byte("{\"version\":99,\"default\":{}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(futurePath); err == nil {
		t.Error("版本过高应报错")
	}
}
