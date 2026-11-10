package pool

import (
	"path/filepath"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestScopeDefaultIsText 未标注的账号一律按文本号池处理。
//
// 这是刻意的保守缺省：新导入的号默认按文本使用（与分区前的行为一致），要拿它画图
// 必须显式标成 media。反过来（缺省 media）会让所有老部署的画图请求在升级第一天
// 全部 403。
func TestScopeDefaultIsText(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	if got := p.ScopeOf("u1"); got != ScopeText {
		t.Fatalf("未标注应为 text，got %q", got)
	}
	if got := p.ScopeOf("nope"); got != "" {
		t.Fatalf("不存在的账号应返回空串，got %q", got)
	}
}

// TestScopeOffKeepsEverythingSelectable 分区关闭时（缺省）选号行为与分区前逐字一致：
// 标了 media 的号照样能被文本请求选中。这条是零回归的唯一保证。
func TestScopeOffKeepsEverythingSelectable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "m", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetScope("m", ScopeMedia)
	if p.ScopeEnforced() {
		t.Fatal("缺省应关闭分区")
	}
	if a := p.PickExcludingForRealmScope(nil, "", "", ScopeText); a == nil {
		t.Fatal("分区关闭时 media 号也应能被文本请求选中")
	}
	// CheckScope 同样放行。
	a := p.AuthFor("m")
	if err := p.CheckScope(a, ScopeText); err != nil {
		t.Fatalf("分区关闭时 CheckScope 应放行: %v", err)
	}
}

// TestScopeOnSeparatesPools 分区开启后：文本请求只选 text 号，媒体请求只选 media 号。
func TestScopeOnSeparatesPools(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "t1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "m1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetScope("m1", ScopeMedia)
	p.SetScopeEnforced(true)

	for i := 0; i < 20; i++ {
		if a := p.PickExcludingForRealmScope(nil, "", "", ScopeText); a == nil || a.UID != "t1" {
			t.Fatalf("文本请求应只选到 t1，got %+v", a)
		}
		if a := p.PickExcludingForRealmScope(nil, "", "", ScopeMedia); a == nil || a.UID != "m1" {
			t.Fatalf("媒体请求应只选到 m1，got %+v", a)
		}
	}
}

// TestScopeEmptyMeansNoFilter scope 传空 = 不过滤（老调用点不传即不受影响）。
func TestScopeEmptyMeansNoFilter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "t1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "m1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetScope("m1", ScopeMedia)
	p.SetScopeEnforced(true)

	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		if a := p.PickExcludingForRealmScope(nil, "", "", ""); a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["t1"] || !seen["m1"] {
		t.Fatalf("scope 为空应不过滤，两个号都该被选中: %v", seen)
	}
}

// TestScopeBackdoorPathsBlocked ⭐ 核心：绕过 pick 的几条路径也必须拦住。
//
// 会话粘性 / 面板钉号直接把 uid 塞进请求，不过 pick()。只在 pick 里过滤等于给媒体号
// 留了后门——参照实现的原话。这里钉住 PickByUID* 与 CheckScope 三条入口。
func TestScopeBackdoorPathsBlocked(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "m1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetScope("m1", ScopeMedia)
	p.SetScopeEnforced(true)

	if got := p.PickByUIDForModelScope("m1", "", ScopeText); got != nil {
		t.Fatal("粘性号路径：媒体号不该承接文本请求")
	}
	if got := p.PickByUIDScope("m1", ScopeText); got != nil {
		t.Fatal("直接钉号路径：媒体号不该承接文本请求")
	}
	if err := p.CheckScope(p.AuthFor("m1"), ScopeText); err == nil {
		t.Fatal("CheckScope 必须拦住媒体号跑文本")
	} else if !IsScopeViolation(err) {
		t.Fatalf("应返回 *ScopeViolation，got %T: %v", err, err)
	}
	// 同号跑媒体请求照常放行。
	if got := p.PickByUIDForModelScope("m1", "", ScopeMedia); got == nil {
		t.Fatal("媒体号应能承接媒体请求")
	}
	if err := p.CheckScope(p.AuthFor("m1"), ScopeMedia); err != nil {
		t.Fatalf("同池应放行: %v", err)
	}
}

// TestScopeFallbackPathRespectsScope 全冷却兜底是选号的最后一道，越界号同样不能进来。
func TestScopeFallbackPathRespectsScope(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "t1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "m1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetScope("m1", ScopeMedia)
	p.SetScopeEnforced(true)
	// 两个号都进冷却 → 主路径无候选，走兜底。
	p.Cooldown("t1", CoolSoft, 3600*1e9, "test")
	p.Cooldown("m1", CoolSoft, 3600*1e9, "test")

	// 兜底只应捞回同池的号；文本池里只剩 t1。
	if a := p.PickExcludingForRealmScope(nil, "", "", ScopeText); a == nil || a.UID != "t1" {
		t.Fatalf("兜底也不能跨池，got %+v", a)
	}
}

// TestScopePersistRoundTrip 号池标注必须跨重启存活：它决定这个号能不能接单，
// 重启后丢失会静默改变路由（媒体号突然开始跑对话）。
func TestScopePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")
	p := New(stateFp)
	p.Add(&auth.Auth{UID: "m1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "t1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetScope("m1", ScopeMedia)
	p.Flush()

	// New 会从 state.json 恢复运行态；Add 是 upsert——保留已恢复的状态、只更新凭证
	// （与 TestCooldownPersists 同一条路径，直接 SyncToDir(nil) 会把账号全删掉）。
	p2 := New(stateFp)
	p2.Add(&auth.Auth{UID: "m1", AccessToken: "at", ExpiresAt: 9999999999})
	p2.Add(&auth.Auth{UID: "t1", AccessToken: "at", ExpiresAt: 9999999999})
	if got := p2.ScopeOf("m1"); got != ScopeMedia {
		t.Fatalf("重启后 m1 应仍是 media，got %q", got)
	}
	if got := p2.ScopeOf("t1"); got != ScopeText {
		t.Fatalf("重启后 t1 应回落 text，got %q", got)
	}
}

// TestSetScopeClearsToDefault 空串 / "text" 都清成缺省（不落盘噪音值），未知值归一 text。
func TestSetScopeClearsToDefault(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetScope("u1", ScopeMedia)
	if got := p.ScopeOf("u1"); got != ScopeMedia {
		t.Fatalf("got %q", got)
	}
	p.SetScope("u1", "")
	if got := p.ScopeOf("u1"); got != ScopeText {
		t.Fatalf("清空后应回落 text，got %q", got)
	}
	if !p.SetScope("u1", "乱写的值") {
		t.Fatal("账号存在时应返回 true")
	}
	if got := p.ScopeOf("u1"); got != ScopeText {
		t.Fatalf("未知值应归一成 text（面板侧另有一层取值校验），got %q", got)
	}
	if p.SetScope("nope", ScopeMedia) {
		t.Fatal("账号不存在应返回 false")
	}
}
