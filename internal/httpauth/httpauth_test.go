package httpauth

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		{"前缀大小写不符（规范要求精确）", "sk-abc123", "bearer sk-abc123", false},
		{"多余空格", "sk-abc123", "Bearer  sk-abc123", false},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}

// ---------------------------------------------------------------------------
// 命名 key 注册表
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewKeyStore(filepath.Join(t.TempDir(), "api_keys.json"))
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	return s
}

func boolPtr(v bool) *bool { return &v }

// TestKeyStoreCreateAuthenticate 覆盖创建→认证→每 key 模型白名单解析。
func TestKeyStoreCreateAuthenticate(t *testing.T) {
	s := newTestStore(t)
	v, secret, err := s.Create("ci", Policy{
		Models: []string{" glm-5.2 ", "cn:glm-5.2", "global:gpt-5.4", ""},
		RPM:    10, MaxConcurrent: 2, DailyCredits: 5,
	}, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(secret, secretPrefix) {
		t.Fatalf("secret 前缀不对: %q", secret)
	}
	if v.Hint == "" || strings.Contains(v.Hint, secret) {
		t.Fatalf("hint 不该为空或包含完整 secret: %q", v.Hint)
	}
	// 名称去空白、模型条目去空/去重。
	if len(v.Policy.Models) != 3 {
		t.Fatalf("models 归一化后应为 3 条，got %v", v.Policy.Models)
	}

	p, err := s.Authenticate(secret, time.Now())
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.KeyID != v.ID || p.Name != "ci" {
		t.Fatalf("身份不匹配: %+v vs %+v", p, v)
	}
	if !p.Restricted() {
		t.Error("配了模型白名单应 Restricted")
	}
	// 裸名两域生效；带前缀只该域生效。
	for _, c := range []struct {
		realm, bare string
		want        bool
	}{
		{"cn", "glm-5.2", true},
		{"global", "glm-5.2", true},
		{"global", "gpt-5.4", true},
		{"cn", "gpt-5.4", false},
		{"cn", "other", false},
		{"cn", "", false},
	} {
		if got := p.Allows(c.realm, c.bare); got != c.want {
			t.Errorf("Allows(%q,%q)=%v want %v", c.realm, c.bare, got, c.want)
		}
	}
	if _, err := s.Authenticate("wb2a_wrong", time.Now()); err != ErrUnauthorized {
		t.Fatalf("错误 secret 应 ErrUnauthorized，got %v", err)
	}

	// 列表视图绝不能带摘要。
	raw, err := json.Marshal(s.List())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hash") || strings.Contains(string(raw), secret) {
		t.Fatalf("View 泄漏了摘要/明文: %s", raw)
	}
}

// TestKeyStorePersistsOnlyHash 落盘只有摘要、权限 0600，重开仍可认证。
func TestKeyStorePersistsOnlyHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "api_keys.json")
	s, err := NewKeyStore(path)
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	_, secret, err := s.Create("disk", Policy{}, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("明文 secret 不得落盘")
	}
	if !strings.Contains(string(raw), hashToken(secret)) {
		t.Fatal("落盘应包含 secret 的 SHA-256 摘要")
	}
	if st, serr := os.Stat(path); serr != nil {
		t.Fatalf("stat: %v", serr)
	} else if st.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600，got %v", st.Mode().Perm())
	}
	reopened, err := NewKeyStore(path)
	if err != nil {
		t.Fatalf(" reopen: %v", err)
	}
	if _, err := reopened.Authenticate(secret, time.Now()); err != nil {
		t.Fatalf("重开注册表后应仍能认证: %v", err)
	}
}

// TestKeyStoreExpiryDisabledDelete 到期/禁用逐请求判定，删除后立刻失效。
func TestKeyStoreExpiryDisabledDelete(t *testing.T) {
	s := newTestStore(t)
	v, secret, err := s.Create("short", Policy{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Authenticate(secret, time.Now()); err != nil {
		t.Fatalf("未到期应通过: %v", err)
	}
	if _, err := s.Authenticate(secret, time.Now().Add(2*time.Hour)); err != ErrExpired {
		t.Fatalf("过期应 ErrExpired，got %v", err)
	}

	if _, err := s.Update(v.ID, UpdateInput{Disabled: boolPtr(true)}); err != nil {
		t.Fatalf("Update disabled: %v", err)
	}
	if _, err := s.Authenticate(secret, time.Now()); err != ErrDisabled {
		t.Fatalf("禁用应 ErrDisabled，got %v", err)
	}
	if _, err := s.Update(v.ID, UpdateInput{Disabled: boolPtr(false), ClearExpiry: true}); err != nil {
		t.Fatalf("Update enable: %v", err)
	}
	if _, err := s.Authenticate(secret, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("清到期+解禁后应通过: %v", err)
	}

	if err := s.Delete(v.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Authenticate(secret, time.Now()); err != ErrUnauthorized {
		t.Fatalf("删除后应 ErrUnauthorized，got %v", err)
	}
	if err := s.Delete(v.ID); err != ErrNotFound {
		t.Fatalf("重复删除应 ErrNotFound，got %v", err)
	}
	if !s.Empty() {
		t.Error("删除后应为空表")
	}
}

// TestKeyStoreRotateGrace 轮换后新旧 secret 在宽限期内都认，宽限期过旧的不认。
func TestKeyStoreRotateGrace(t *testing.T) {
	s := newTestStore(t)
	v, oldSecret, err := s.Create("rotate", Policy{}, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, newSecret, err := s.Rotate(v.ID, time.Hour)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if newSecret == oldSecret {
		t.Fatal("轮换必须换 secret")
	}
	now := time.Now()
	for _, tok := range []string{oldSecret, newSecret} {
		if _, err := s.Authenticate(tok, now); err != nil {
			t.Fatalf("宽限期内 %q 应通过: %v", tok, err)
		}
	}
	// 极短宽限：过期后旧 secret 失效、新 secret 仍有效。
	if _, _, err := s.Rotate(v.ID, time.Millisecond); err != nil {
		t.Fatalf("Rotate short: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := s.Authenticate(newSecret, time.Now()); err != ErrUnauthorized {
		t.Fatalf("上个 secret 宽限过后应失效，got %v", err)
	}
}

// TestPrincipalAdmitRPM 60s 固定窗口内的请求数上限。
func TestPrincipalAdmitRPM(t *testing.T) {
	s := newTestStore(t)
	_, secret, err := s.Create("rpm", Policy{RPM: 2}, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Now()
	auth := func(at time.Time) *Principal {
		p, err := s.Authenticate(secret, at)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		return p
	}
	for i := 0; i < 2; i++ {
		p := auth(now)
		if err := p.Admit(now); err != nil {
			t.Fatalf("第 %d 次应通过: %v", i+1, err)
		}
		p.Release() // 没配并发上限，Release 是 no-op
	}
	if err := auth(now).Admit(now); err != ErrRateLimited {
		t.Fatalf("超 RPM 应 ErrRateLimited，got %v", err)
	}
	// 窗口滚动后恢复。
	if err := auth(now.Add(61 * time.Second)).Admit(now.Add(61 * time.Second)); err != nil {
		t.Fatalf("新窗口应通过: %v", err)
	}
}

// TestPrincipalAdmitConcurrency 并发在途上限；释放幂等，槽位随后可复用。
func TestPrincipalAdmitConcurrency(t *testing.T) {
	s := newTestStore(t)
	_, secret, err := s.Create("conc", Policy{MaxConcurrent: 1}, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Now()
	auth := func() *Principal {
		p, err := s.Authenticate(secret, now)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		return p
	}

	p1 := auth()
	if err := p1.Admit(now); err != nil {
		t.Fatalf("第 1 个在途应通过: %v", err)
	}
	if err := auth().Admit(now); err != ErrConcurrent {
		t.Fatalf("并发超限应 ErrConcurrent，got %v", err)
	}
	p1.Release()
	p1.Release() // 幂等
	if err := auth().Admit(now); err != nil {
		t.Fatalf("释放后应可再次准入: %v", err)
	}
}

// TestPrincipalDailyBudget 每 CST 自然日累计 credit；到顶拒绝，跨日清零。
func TestPrincipalDailyBudget(t *testing.T) {
	s := newTestStore(t)
	_, secret, err := s.Create("budget", Policy{DailyCredits: 2}, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Now()
	auth := func(at time.Time) *Principal {
		p, err := s.Authenticate(secret, at)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		return p
	}

	p := auth(now)
	if used, limit, ok := p.AdmitBudget(now); !ok || used != 0 || limit != 2 {
		t.Fatalf("初始预算应放行: used=%v limit=%v ok=%v", used, limit, ok)
	}
	p.Charge(1.5, true, now)
	p.Charge(99, false, now) // usage 缺失：不计
	if used, _, ok := p.AdmitBudget(now); !ok || used != 1.5 {
		t.Fatalf("用 1.5 后应仍放行: used=%v ok=%v", used, ok)
	}
	p.Charge(0.5, true, now)
	if used, _, ok := p.AdmitBudget(now); ok || used != 2 {
		t.Fatalf("到顶应拒绝: used=%v ok=%v", used, ok)
	}
	// 次日 00:00 CST 之后清零（运行期计数按 key 共享，重新认证也读同一份）。
	nextDay := auth(now.Add(24 * time.Hour))
	if used, _, ok := nextDay.AdmitBudget(now.Add(24 * time.Hour)); !ok || used != 0 {
		t.Fatalf("跨日应清零: used=%v ok=%v", used, ok)
	}
}

// TestMasterPrincipalUnrestricted 主口令不受任何 per-key 策略约束。
func TestMasterPrincipalUnrestricted(t *testing.T) {
	m := MasterPrincipal()
	if m == nil || m.Restricted() {
		t.Fatal("主口令不该有模型白名单")
	}
	if !m.Allows("cn", "anything") {
		t.Fatal("主口令应放行全部模型")
	}
	if err := m.Admit(time.Now()); err != nil {
		t.Fatalf("主口令不该被 RPM/并发拦: %v", err)
	}
	if _, _, ok := m.AdmitBudget(time.Now()); !ok {
		t.Fatal("主口令不该有日预算")
	}
	m.Charge(10, true, time.Now()) // 必须 no-op 且不 panic
	m.Release()
}

// TestPolicyValidation 负值/非法策略必须报错，而不是静默钳成「不限」。
func TestPolicyValidation(t *testing.T) {
	s := newTestStore(t)
	for _, pol := range []Policy{
		{RPM: -1},
		{MaxConcurrent: -1},
		{DailyCredits: -0.5},
	} {
		if _, _, err := s.Create("bad", pol, time.Time{}); err == nil {
			t.Fatalf("策略 %+v 应被拒绝", pol)
		}
	}
	if _, _, err := s.Create("bad", Policy{DailyCredits: math.Inf(1)}, time.Time{}); err == nil {
		t.Fatal("+Inf 预算应被拒绝")
	}
	if _, _, err := s.Create("bad", Policy{DailyCredits: math.NaN()}, time.Time{}); err == nil {
		t.Fatal("NaN 预算应被拒绝")
	}
}

// TestTokenMatches 主口令比较与 VerifyBearer 同口径（常量时间、等长摘要）。
func TestTokenMatches(t *testing.T) {
	if !TokenMatches("sk-abc", "sk-abc") {
		t.Error("相同 token 应匹配")
	}
	if TokenMatches("sk-abc", "sk-abd") || TokenMatches("sk-abc", "") {
		t.Error("不同/空期望值不得匹配")
	}
}
