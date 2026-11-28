package ratelimit

import "testing"

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", -1},
		{"0", 0},
		{"1", 1},
		{"0.5", 0.5},
		{" 1.5x ", 1.5},
		{"FREE", -1},
		{"abc", -1},
	}
	for _, c := range cases {
		if got := ParseRate(c.in); got != c.want {
			t.Errorf("ParseRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// 这两个数是「跟随上游」的全部依据：改了它们等于改了所有跟随口径的额度。
func TestFollowTokenQuota(t *testing.T) {
	cases := []struct {
		realm string
		want  int64
	}{
		{"cn", 200_000_000},
		{"CN", 200_000_000},
		{"global", 100_000_000},
		{"", 200_000_000},
		{"mars", 200_000_000},
	}
	for _, c := range cases {
		if got := FollowTokenQuota(c.realm); got != c.want {
			t.Errorf("FollowTokenQuota(%q) = %d, want %d", c.realm, got, c.want)
		}
	}
}

func TestNormalizeDefaults(t *testing.T) {
	n := Normalize(ModelLimit{})
	if n.Source != SourceFollow {
		t.Errorf("零值来源应为跟随上游，得到 %q", n.Source)
	}
	if n.OnHit != OnHitDegrade {
		t.Errorf("零值命中动作为降级，得到 %q", n.OnHit)
	}
	if n.ChainMode != ChainAuto {
		t.Errorf("零值链模式应为 auto，得到 %q", n.ChainMode)
	}
	if n.WindowSec != DefaultWindowSec {
		t.Errorf("零值窗口应为 %d，得到 %d", DefaultWindowSec, n.WindowSec)
	}
	m := Normalize(ModelLimit{Model: "cn:deepseek-v4.1-flash"})
	if m.Realm != "cn" || m.Model != "deepseek-v4.1-flash" {
		t.Errorf("带域前缀的模型名应被拆开，得到 realm=%q model=%q", m.Realm, m.Model)
	}
	if got := Normalize(ModelLimit{Source: " Manual "}).Source; got != SourceManual {
		t.Errorf("来源应大小写与空白不敏感，得到 %q", got)
	}
}

func TestResolveQuota(t *testing.T) {
	// 跟随上游：token 维度走推算，池级 = 单账号 x 可路由号数
	q := ResolveQuota(Normalize(ModelLimit{}), "cn", 3)
	if !q.Inferred {
		t.Error("零配额应标记为推算口径")
	}
	if q.TokenPerAccount != 200_000_000 || q.TokenPerPool != 600_000_000 {
		t.Errorf("cn 推算额度应为 200M x3，得到单号 %d 池级 %d", q.TokenPerAccount, q.TokenPerPool)
	}
	if q.WindowSec != DefaultWindowSec {
		t.Errorf("窗口应为 %d，得到 %d", DefaultWindowSec, q.WindowSec)
	}

	// 手动配额：不算推算
	q2 := ResolveQuota(Normalize(ModelLimit{TokenQuotaPerAccount: 1_000_000}), "cn", 4)
	if q2.Inferred {
		t.Error("手动配额不应标记为推算")
	}
	if q2.TokenPerPool != 4_000_000 {
		t.Errorf("池级配额应为 4M，得到 %d", q2.TokenPerPool)
	}

	// 没有可路由账号：池级为 0（页面据此显示 locked，而不是除零）
	if q3 := ResolveQuota(Normalize(ModelLimit{}), "cn", 0); q3.TokenPerPool != 0 {
		t.Errorf("无可路由账号时池级配额应为 0，得到 %d", q3.TokenPerPool)
	}
	if q4 := ResolveQuota(Normalize(ModelLimit{}), "cn", -2); q4.TokenPerPool != 0 {
		t.Errorf("负号数应按 0 处理，得到 %d", q4.TokenPerPool)
	}

	// 积分维度：只给单账号预算时，池级按号数推出
	q5 := ResolveQuota(Normalize(ModelLimit{CreditQuotaPerAccount: 10}), "cn", 2)
	if q5.CreditPerPool != 20 {
		t.Errorf("池级积分预算应为 20，得到 %v", q5.CreditPerPool)
	}
	// 显式池级预算不被覆盖
	q6 := ResolveQuota(Normalize(ModelLimit{CreditQuotaPerAccount: 10, CreditQuotaPerPool: 3}), "cn", 2)
	if q6.CreditPerPool != 3 {
		t.Errorf("显式池级积分预算应保留 3，得到 %v", q6.CreditPerPool)
	}
}

func TestEffectiveLimitPrecedence(t *testing.T) {
	p := Policy{
		Default: Normalize(ModelLimit{TokenQuotaPerAccount: 1000, Source: SourceFollow}),
		Overrides: []ModelLimit{
			{Realm: "cn", Model: "m1", TokenQuotaPerAccount: 300},
			{Realm: "cn", TokenQuotaPerAccount: 200},
			{Model: "m1", TokenQuotaPerAccount: 100},
		},
	}
	cases := []struct {
		realm, model string
		want         int64
		why          string
	}{
		{"cn", "m1", 300, "域+模型最具体"},
		{"cn", "m2", 200, "域级覆盖压过模型级"},
		{"global", "m1", 100, "模型级在该域生效"},
		{"global", "m2", 1000, "只剩默认值"},
	}
	for _, c := range cases {
		if got := EffectiveLimit(p, c.realm, c.model).TokenQuotaPerAccount; got != c.want {
			t.Errorf("EffectiveLimit(%s,%s) = %d, want %d（%s）", c.realm, c.model, got, c.want, c.why)
		}
	}

	// 覆盖项的书写顺序不该改变生效口径：否则「挪了个位置就变行为」无法解释。
	shuffled := Policy{Default: p.Default, Overrides: []ModelLimit{p.Overrides[2], p.Overrides[0], p.Overrides[1]}}
	if got := EffectiveLimit(shuffled, "cn", "m1").TokenQuotaPerAccount; got != 300 {
		t.Errorf("打乱覆盖项顺序后生效额度变了：%d", got)
	}
}

func TestValidate(t *testing.T) {
	dup := Policy{
		Default:   Normalize(ModelLimit{}),
		Overrides: []ModelLimit{{Realm: "cn", Model: "m1"}, {Realm: "CN", Model: "M1"}},
	}
	if err := Validate(dup); err == nil {
		t.Error("重复作用域应校验失败")
	}
	bad := []ModelLimit{
		{Realm: "mars"},
		{TokenQuotaPerAccount: -1},
		{CreditQuotaPerPool: -1},
		{WindowSec: 1},
	}
	for _, l := range bad {
		if err := ValidateLimit(Normalize(l)); err == nil {
			t.Errorf("非法策略应校验失败：%+v", l)
		}
	}
	if err := Validate(DefaultPolicy()); err != nil {
		t.Errorf("出厂默认策略应合法，得到 %v", err)
	}
}

func TestApplyPreset(t *testing.T) {
	p, err := ApplyPreset(DefaultPolicy(), "saver")
	if err != nil {
		t.Fatalf("套用省钱预设失败：%v", err)
	}
	if p.Preset != "saver" {
		t.Errorf("预设 id 应为 saver，得到 %q", p.Preset)
	}
	if p.Default.Source != SourceManual || p.Default.TokenQuotaPerAccount != 50_000_000 {
		t.Errorf("省钱预设应是手动口径 + 50M，得到 %+v", p.Default)
	}
	if len(p.Overrides) != 1 || p.Overrides[0].Realm != "global" || p.Overrides[0].TokenQuotaPerAccount != 25_000_000 {
		t.Fatalf("省钱预设应含 global 25M 覆盖项，得到 %+v", p.Overrides)
	}
	if got := EffectiveLimit(p, "global", "x").TokenQuotaPerAccount; got != 25_000_000 {
		t.Errorf("global 域生效额度应为 25M，得到 %d", got)
	}
	if got := EffectiveLimit(p, "cn", "x").TokenQuotaPerAccount; got != 50_000_000 {
		t.Errorf("cn 域生效额度应为 50M，得到 %d", got)
	}

	q, err := ApplyPreset(DefaultPolicy(), "quality")
	if err != nil {
		t.Fatalf("套用质量预设失败：%v", err)
	}
	if q.Default.OnHit != OnHitHold || q.Default.ChainMode != ChainOff || q.Default.Source != SourceManual {
		t.Errorf("质量预设应是手动 + 直接拦 + 不降级，得到 %+v", q.Default)
	}

	if _, err := ApplyPreset(p, "nope"); err == nil {
		t.Error("未知预设应报错")
	}
	if dp := DefaultPolicy(); dp.Preset != "follow" {
		t.Errorf("出厂默认预设应为 follow，得到 %q", dp.Preset)
	}
}
