package pool

import (
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// 模型规则：判定语义（deny / allow / deny 优先 / 按域隔离 / 裸名兼容）
// ---------------------------------------------------------------------------

func TestModelRuleSemantics(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1"})
	p.Add(&auth.Auth{UID: "gl1", Domain: "x.workbuddy.ai"}) // .workbuddy.ai → global

	deny, err := p.UpsertRule(ModelRule{Name: "d", Mode: RuleModeDeny, Models: []string{"cn:hy4std"}})
	if err != nil {
		t.Fatalf("新建黑名单失败: %v", err)
	}
	allow, err := p.UpsertRule(ModelRule{Name: "a", Mode: RuleModeAllow, Models: []string{"deepseek-v4.1-flash"}})
	if err != nil {
		t.Fatalf("新建白名单失败: %v", err)
	}

	// 黑名单：规则里带的模型不可用；带不带前缀查询都应命中（选号链路传裸名）。
	if _, err := p.AssignRules("cn1", []string{deny.ID}); err != nil {
		t.Fatalf("挂规则失败: %v", err)
	}
	if !p.byUID["cn1"].blockedByRules("hy4std") || !p.byUID["cn1"].blockedByRules("cn:hy4std") {
		t.Error("黑名单应拦住裸名与前缀名")
	}
	if p.byUID["cn1"].blockedByRules("glm-5.3") {
		t.Error("不在黑名单里的模型不该被拦")
	}
	// 按域隔离：cn: 前缀的规则不影响 global 账号。
	if p.byUID["gl1"].blockedByRules("hy4std") {
		t.Error("cn: 前缀的规则不该影响 global 账号")
	}

	// 白名单：只放行列表内。
	if _, err := p.AssignRules("cn1", []string{allow.ID}); err != nil {
		t.Fatalf("挂白名单失败: %v", err)
	}
	if p.byUID["cn1"].blockedByRules("deepseek-v4.1-flash") {
		t.Error("白名单内的模型应放行")
	}
	if !p.byUID["cn1"].blockedByRules("glm-5.3") {
		t.Error("白名单外的模型应拦住")
	}

	// 黑白同时挂：deny 优先，allow 仍放行未被 deny 的模型。
	if _, err := p.AssignRules("cn1", []string{deny.ID, allow.ID}); err != nil {
		t.Fatalf("同时挂两条失败: %v", err)
	}
	if !p.byUID["cn1"].blockedByRules("hy4std") {
		t.Error("deny 应优先于 allow")
	}
	if p.byUID["cn1"].blockedByRules("deepseek-v4.1-flash") {
		t.Error("allow 仍应放行未 deny 的模型")
	}

	// 空模型名不做模型级判断（与 modelOffed 口径一致）。
	if p.byUID["cn1"].blockedByRules("") {
		t.Error("空模型名应放行")
	}
}

// ---------------------------------------------------------------------------
// 模型规则：批量挂/摘（规则卡片「应用」）、删规则摘引用
// ---------------------------------------------------------------------------

func TestModelRuleApply(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	r, err := p.UpsertRule(ModelRule{Name: "r", Mode: RuleModeDeny, Models: []string{"m1"}})
	if err != nil {
		t.Fatalf("新建规则失败: %v", err)
	}

	if n, err := p.ApplyRule(r.ID, []string{"u1", "u2"}, true); err != nil || n != 2 {
		t.Fatalf("首次 apply 应改 2 个账号，得到 %d, %v", n, err)
	}
	if n, _ := p.ApplyRule(r.ID, []string{"u1", "u2"}, true); n != 0 {
		t.Errorf("重复 apply 应无变化，得到 %d", n)
	}
	if n, _ := p.ApplyRule(r.ID, []string{"u1"}, false); n != 1 {
		t.Errorf("摘掉一个应改 1 个，得到 %d", n)
	}
	if got := p.RuleIDsOf("u1"); len(got) != 0 {
		t.Errorf("u1 应已摘空，得到 %v", got)
	}
	if !p.byUID["u2"].blockedByRules("m1") {
		t.Error("u2 仍应被拦")
	}

	// 删规则：所有账号上的引用一起摘掉。
	if !p.DeleteRule(r.ID) {
		t.Fatal("删除规则应成功")
	}
	if got := p.RuleIDsOf("u2"); len(got) != 0 {
		t.Errorf("删规则后引用应清空，得到 %v", got)
	}
	if p.byUID["u2"].blockedByRules("m1") {
		t.Error("删规则后不该再拦")
	}

	// 野 id 不能写进账号。
	if _, err := p.ApplyRule("nope", []string{"u2"}, true); err == nil {
		t.Error("不存在的规则 id 应报错")
	}
}

// ---------------------------------------------------------------------------
// 模型规则：落盘与重启恢复（先账号后规则，recompute 不能把引用清掉）
// ---------------------------------------------------------------------------

func TestModelRulePersist(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	r, err := p.UpsertRule(ModelRule{Name: "r", Mode: RuleModeDeny, Models: []string{"cn:hy4std"}})
	if err != nil {
		t.Fatalf("新建规则失败: %v", err)
	}
	if _, err := p.AssignRules("u1", []string{r.ID}); err != nil {
		t.Fatalf("挂规则失败: %v", err)
	}

	p.mu.RLock()
	sf := p.stateOverviewLocked()
	p.mu.RUnlock()
	if len(sf.ModelRules) != 1 || len(sf.Accounts) != 1 || len(sf.Accounts["u1"].ModelRules) != 1 {
		t.Fatalf("落盘结构不对：rules=%d accounts=%d", len(sf.ModelRules), len(sf.Accounts))
	}

	// 模拟重启：装载顺序是先账号（带着 id）后规则本体。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.Lock()
	p2.applyAccountsLocked(sf.Accounts)
	p2.applyRulesLocked(sf.ModelRules)
	p2.mu.Unlock()

	if got := p2.RuleIDsOf("u1"); len(got) != 1 {
		t.Fatalf("重启后账号挂的规则丢失：%v", got)
	}
	if !p2.byUID["u1"].blockedByRules("hy4std") {
		t.Error("重启后应仍拦住 hy4std")
	}
}

// ---------------------------------------------------------------------------
// 回归：跨域白名单「一个模型都没命中」时不能把账号封死
// ---------------------------------------------------------------------------

// 典型误配：global 账号挂了纯 cn: 的白名单。早先实现先把 ruleHasAllow 置了真、
// 再按域过滤，于是 ruleAllow 为空 → 该号对所有模型都不可用，而面板上它只是
// 「挂了一条白名单」，完全看不出异常。现在：本域命中 0 个模型 = 这条规则没挂。
func TestModelRuleAllowCrossRealmIsNoop(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "gl1", Domain: "x.workbuddy.ai"})

	r, err := p.UpsertRule(ModelRule{Name: "cn-only", Mode: RuleModeAllow, Models: []string{"cn:hy4std", "cn:glm-5.3"}})
	if err != nil {
		t.Fatalf("新建白名单失败: %v", err)
	}
	if _, err := p.AssignRules("gl1", []string{r.ID}); err != nil {
		t.Fatalf("挂规则失败: %v", err)
	}
	e := p.byUID["gl1"]
	if e.ruleHasAllow {
		t.Error("跨域白名单一个模型都没命中，不该置 ruleHasAllow")
	}
	for _, m := range []string{"hy4std", "glm-5.3", "gpt-5.6-sol"} {
		if e.blockedByRules(m) {
			t.Errorf("跨域白名单不该拦住 %s", m)
		}
	}
	// 同一条规则挂到 cn 账号上仍是有效白名单。
	p.Add(&auth.Auth{UID: "cn1"})
	if _, err := p.AssignRules("cn1", []string{r.ID}); err != nil {
		t.Fatalf("挂规则失败: %v", err)
	}
	if !p.byUID["cn1"].ruleHasAllow || !p.byUID["cn1"].blockedByRules("gpt-5.6-sol") {
		t.Error("cn 账号上这条白名单应生效")
	}
}

// ---------------------------------------------------------------------------
// 回归：显式带未知 id 的保存要报错，而不是悄悄建一条新规则
// ---------------------------------------------------------------------------

func TestModelRuleUpsertUnknownID(t *testing.T) {
	p := New("")
	if _, err := p.UpsertRule(ModelRule{ID: "nope", Name: "x", Mode: RuleModeDeny, Models: []string{"hy4std"}}); err == nil {
		t.Fatal("未知 id 应报错")
	}
	if len(p.Rules()) != 0 {
		t.Fatalf("未知 id 不该建出规则，得到 %d 条", len(p.Rules()))
	}
	existing, err := p.UpsertRule(ModelRule{Name: "d", Mode: RuleModeDeny, Models: []string{"hy4std"}})
	if err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	upd, err := p.UpsertRule(ModelRule{ID: existing.ID, Name: "d2", Mode: RuleModeDeny, Models: []string{"hy4std", "glm-5.3"}})
	if err != nil {
		t.Fatalf("改已有规则失败: %v", err)
	}
	if upd.Name != "d2" || len(upd.Models) != 2 {
		t.Fatalf("改已有规则没生效: %+v", upd)
	}
}
