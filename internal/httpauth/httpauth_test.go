package httpauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
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

// TestBearerToken 客户端密钥要拿原文查表，解析口径必须与 VerifyBearer 一致：
// 只认精确的 "Bearer " 前缀，缺头/方案不符返回空串。
func TestBearerToken(t *testing.T) {
	cases := []struct{ authz, want string }{
		{"", ""},
		{"Bearer sk-abc", "sk-abc"},
		{"Bearer  sk-abc", "sk-abc"}, // 与 VerifyBearer 不同：这里裁掉多余空格再查表
		{"bearer sk-abc", ""},
		{"Basic sk-abc", ""},
		{"Bearer ", ""},
	}
	for _, c := range cases {
		if got := BearerToken(req(c.authz)); got != c.want {
			t.Errorf("BearerToken(authz=%q) = %q, want %q", c.authz, got, c.want)
		}
	}
}

// newTestKey 建一把启用的密钥（内存表，测试不落盘）。
func newTestKey(t *testing.T, in ClientKeyInput) (*ClientKeyStore, ClientKey) {
	t.Helper()
	s, err := OpenClientKeys("")
	if err != nil {
		t.Fatalf("OpenClientKeys: %v", err)
	}
	if !in.Enabled {
		in.Enabled = true
	}
	k, err := s.Create(in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s, k
}

// TestClientKeyEmptyStoreAdmitsNothing 一把密钥都没有时 Admit 返回 (nil, nil)：
// 调用方据此维持旧「不鉴权」语义（零回归）。
func TestClientKeyEmptyStoreAdmitsNothing(t *testing.T) {
	s, _ := OpenClientKeys("")
	if s.HasKeys() {
		t.Fatal("新表不该有密钥")
	}
	lease, denied := s.Admit("whatever", time.Now())
	if lease != nil || denied != nil {
		t.Fatalf("空表应返回 (nil, nil)，got (%v, %v)", lease, denied)
	}
	var nilStore *ClientKeyStore
	if nilStore.HasKeys() {
		t.Fatal("nil store 不该报告有密钥")
	}
	if _, d := nilStore.Admit("x", time.Now()); d != nil {
		t.Fatal("nil store 不该拒绝（未启用）")
	}
}

// TestClientKeyCreateShape 密钥原文形如 sk-...，且 id 非空、默认计入 Used/Today。
func TestClientKeyCreateShape(t *testing.T) {
	_, k := newTestKey(t, ClientKeyInput{Name: " 团队A "})
	if !strings.HasPrefix(k.Secret, "sk-") || len(k.Secret) < 20 {
		t.Fatalf("密钥形状不对: %q", k.Secret)
	}
	if k.ID == "" {
		t.Fatal("id 不能为空")
	}
	if k.Name != "团队A" {
		t.Fatalf("名称应裁剪空白，got %q", k.Name)
	}
	if k.CreatedAt.IsZero() {
		t.Fatal("created_at 应填当前时间")
	}
}

// TestClientKeyDailyRequestLimit 日限请求数：第 N+1 次 429，带 Retry-After 到 CST 次日；
// 退还（Refund）后又能用——4xx/5xx 不该占名额。
func TestClientKeyDailyRequestLimit(t *testing.T) {
	s, k := newTestKey(t, ClientKeyInput{Name: "a", DailyLimit: Limits{Requests: 2}})
	now := time.Now()

	for i := 0; i < 2; i++ {
		if lease, denied := s.Admit(k.Secret, now); lease == nil || denied != nil {
			t.Fatalf("第 %d 次应放行，denied=%v", i+1, denied)
		}
	}
	lease, denied := s.Admit(k.Secret, now)
	if lease != nil || denied == nil {
		t.Fatal("超额应拒绝")
	}
	if denied.Status != http.StatusTooManyRequests || denied.Code != "key_quota_exceeded" {
		t.Fatalf("拒绝形状不对: %d %s", denied.Status, denied.Code)
	}
	if denied.RetryAfter <= 0 || denied.RetryAfter > 24*time.Hour {
		t.Fatalf("日限的 Retry-After 应落在 (0, 24h]，got %v", denied.RetryAfter)
	}

	s.Refund(k.ID)
	if lease, denied := s.Admit(k.Secret, now); lease == nil || denied != nil {
		t.Fatalf("退还后应放行，denied=%v", denied)
	}
	got, _ := s.Get(k.ID)
	if got.Today.Requests != 2 || got.Used.Requests != 2 {
		t.Fatalf("用量口径不对: today=%d used=%d, want 2/2", got.Today.Requests, got.Used.Requests)
	}
}

// TestClientKeyTotalLimit 总量用完不带 Retry-After（等不来），且 Refund 不会把计数减成负。
func TestClientKeyTotalLimit(t *testing.T) {
	s, k := newTestKey(t, ClientKeyInput{Name: "a", TotalLimit: Limits{Requests: 1}})
	now := time.Now()
	if _, d := s.Admit(k.Secret, now); d != nil {
		t.Fatalf("第一次应放行: %v", d)
	}
	_, d := s.Admit(k.Secret, now)
	if d == nil || d.Status != http.StatusTooManyRequests {
		t.Fatalf("总量用完应 429，got %v", d)
	}
	if d.RetryAfter != 0 {
		t.Fatalf("总量上限不该带 Retry-After，got %v", d.RetryAfter)
	}
	for i := 0; i < 5; i++ {
		s.Refund(k.ID)
	}
	got, _ := s.Get(k.ID)
	if got.Today.Requests != 0 || got.Used.Requests != 0 {
		t.Fatalf("Refund 不该减成负数: today=%d used=%d", got.Today.Requests, got.Used.Requests)
	}
}

// TestClientKeyDisabledAndExpired 停用/过期都是 401，但码不同（面板/客户端据此区分）。
func TestClientKeyDisabledAndExpired(t *testing.T) {
	now := time.Now()

	s, k := newTestKey(t, ClientKeyInput{Name: "off"})
	if _, err := s.Update(k.ID, ClientKeyInput{Name: "off", Enabled: false}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, d := s.Admit(k.Secret, now); d == nil || d.Code != "key_disabled" || d.Status != http.StatusUnauthorized {
		t.Fatalf("停用应 401 key_disabled，got %v", d)
	}

	s2, k2 := newTestKey(t, ClientKeyInput{Name: "old", ExpiresAt: now.Add(-time.Minute)})
	if _, d := s2.Admit(k2.Secret, now); d == nil || d.Code != "key_expired" {
		t.Fatalf("过期应 key_expired，got %v", d)
	}

	// 边界：到期时刻本身即失效（!now.Before(expires)）。
	s3, k3 := newTestKey(t, ClientKeyInput{Name: "edge", ExpiresAt: now})
	if _, d := s3.Admit(k3.Secret, now); d == nil || d.Code != "key_expired" {
		t.Fatalf("到期时刻应已失效，got %v", d)
	}
}

// TestClientKeyUnknownSecret 表里有密钥但没有这把：401 invalid_api_key。
func TestClientKeyUnknownSecret(t *testing.T) {
	s, _ := newTestKey(t, ClientKeyInput{Name: "a"})
	for _, secret := range []string{"", "sk-nope", " "} {
		if _, d := s.Admit(secret, time.Now()); d == nil || d.Code != "invalid_api_key" {
			t.Fatalf("secret=%q 应 invalid_api_key，got %v", secret, d)
		}
	}
}

// TestClientKeyModelWhitelist 白名单：裸名、大小写不敏感、* 通配；空 = 不限。
func TestClientKeyModelWhitelist(t *testing.T) {
	s, k := newTestKey(t, ClientKeyInput{Name: "a", Models: []string{"GLM-*", "gpt-5.4", " claude-3-* "}})
	lease, d := s.Admit(k.Secret, time.Now())
	if lease == nil || d != nil {
		t.Fatalf("应放行: %v", d)
	}
	for _, m := range []string{"glm-5.2", "GLM-4.6", "gpt-5.4", "GPT-5.4", "claude-3-opus"} {
		if !lease.AllowsModel(m) {
			t.Errorf("%q 应放行", m)
		}
	}
	for _, m := range []string{"gpt-5", "claude-2", "glm"} {
		if lease.AllowsModel(m) {
			t.Errorf("%q 不该放行", m)
		}
	}

	// 空白名单 = 不限；nil lease（管理员/未启用）也不受限。
	s2, k2 := newTestKey(t, ClientKeyInput{Name: "b"})
	l2, _ := s2.Admit(k2.Secret, time.Now())
	if !l2.AllowsModel("anything") {
		t.Error("空白名单应放行任意模型")
	}
	var none *ClientKeyLease
	if !none.AllowsModel("anything") {
		t.Error("nil lease 应放行")
	}
}

// TestClientKeyDayRollover CST 跨日：Today 清零，日限重新可用（Used 继续累计）。
func TestClientKeyDayRollover(t *testing.T) {
	// 用 CST 当天 10:00 作为基准，避免测试跑在 23:5x 时 +24h 仍落在同一天的极端。
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, cst.Loc)
	s, k := newTestKey(t, ClientKeyInput{Name: "a", DailyLimit: Limits{Requests: 1}})
	if _, d := s.Admit(k.Secret, now); d != nil {
		t.Fatalf("应放行: %v", d)
	}
	if _, d := s.Admit(k.Secret, now); d == nil {
		t.Fatal("当日第二次应拒绝")
	}
	if _, d := s.Admit(k.Secret, now.Add(24*time.Hour)); d != nil {
		t.Fatalf("次日应重新放行: %v", d)
	}
	got, _ := s.Get(k.ID)
	if got.Day != cst.Day(now.Add(24*time.Hour)) {
		t.Fatalf("day 应滚到次日，got %q", got.Day)
	}
	if got.Today.Requests != 1 {
		t.Fatalf("Today 应清零后重新计数，got %d", got.Today.Requests)
	}
	if got.Used.Requests != 2 {
		t.Fatalf("Used 不该清零，got %d", got.Used.Requests)
	}
}

// TestClientKeyTokenAndCreditUsage token/积分累加，非正值忽略。
func TestClientKeyTokenAndCreditUsage(t *testing.T) {
	s, k := newTestKey(t, ClientKeyInput{Name: "a"})
	s.AddUsage(k.ID, 100, 0.5)
	s.AddUsage(k.ID, 0, 0)     // 无观测：忽略
	s.AddUsage(k.ID, -5, -1.0) // 脏值：忽略
	got, _ := s.Get(k.ID)
	if got.Used.Tokens != 100 || got.Today.Tokens != 100 {
		t.Fatalf("token 累加不对: used=%d today=%d", got.Used.Tokens, got.Today.Tokens)
	}
	if got.Used.Credits != 0.5 || got.Today.Credits != 0.5 {
		t.Fatalf("积分累加不对: used=%v today=%v", got.Used.Credits, got.Today.Credits)
	}
}

// TestClientKeyPersistAndReload 增删改立即落盘：重开文件后密钥、上限、用量都在；
// 文件权限 0600。
func TestClientKeyPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s, err := OpenClientKeys(path)
	if err != nil {
		t.Fatalf("OpenClientKeys: %v", err)
	}
	k, err := s.Create(ClientKeyInput{
		Name: "team", Note: "note", Enabled: true,
		DailyLimit: Limits{Requests: 10, Tokens: 1000, Credits: 1.5},
		Models:     []string{"glm-*"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, d := s.Admit(k.Secret, time.Now()); d != nil {
		t.Fatalf("Admit: %v", d)
	}
	s.AddUsage(k.ID, 42, 0.25)
	s.Flush()

	re, err := OpenClientKeys(path)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	if !re.HasKeys() {
		t.Fatal("重开后应有密钥")
	}
	got, ok := re.Get(k.ID)
	if !ok {
		t.Fatal("按 id 找不到密钥")
	}
	if got.Secret != k.Secret || got.Name != "team" || got.Note != "note" {
		t.Fatalf("重开后字段不一致: %+v", got)
	}
	if got.DailyLimit.Requests != 10 || got.Used.Tokens != 42 || got.Today.Credits != 0.25 {
		t.Fatalf("上限/用量没落盘: %+v", got)
	}
	if len(got.Models) != 1 || got.Models[0] != "glm-*" {
		t.Fatalf("白名单没落盘: %v", got.Models)
	}
	// 重开后的表能直接用原文准入（索引重建）。
	if _, d := re.Admit(k.Secret, time.Now()); d != nil {
		t.Fatalf("重开后准入失败: %v", d)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("密钥文件权限 = %o, want 600", perm)
	}
}

// TestClientKeyDeleteAndUpdate 删除后该密钥立即失效；Update 不动原文与累计用量。
func TestClientKeyDeleteAndUpdate(t *testing.T) {
	s, k := newTestKey(t, ClientKeyInput{Name: "a"})
	if _, d := s.Admit(k.Secret, time.Now()); d != nil {
		t.Fatalf("Admit: %v", d)
	}
	upd, err := s.Update(k.ID, ClientKeyInput{Name: "b", Enabled: true, Models: []string{"gpt-*"}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Secret != k.Secret {
		t.Fatal("Update 不该换密钥原文")
	}
	if upd.Used.Requests != 1 {
		t.Fatalf("Update 不该清累计用量，got %d", upd.Used.Requests)
	}
	if _, err := s.Update("nope", ClientKeyInput{Name: "x"}); err == nil {
		t.Fatal("改不存在的密钥应报错")
	}
	if err := s.Delete(k.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.HasKeys() {
		t.Fatal("删光后不该报告有密钥")
	}
	if _, d := s.Admit(k.Secret, time.Now()); d != nil {
		t.Fatalf("删光后旧密钥应回到「未启用」语义（nil,nil），got %v", d)
	}
	if err := s.Delete(k.ID); err == nil {
		t.Fatal("重复删除应报错")
	}
}

// TestClientKeyValidation 面板输入校验：空名、超长、负上限都拒绝。
func TestClientKeyValidation(t *testing.T) {
	s, _ := OpenClientKeys("")
	for _, in := range []ClientKeyInput{
		{Name: "   "},
		{Name: strings.Repeat("x", maxKeyName+1)},
		{Name: "a", Note: strings.Repeat("x", maxKeyNote+1)},
		{Name: "a", DailyLimit: Limits{Requests: -1}},
		{Name: "a", TotalLimit: Limits{Credits: -0.5}},
	} {
		if _, err := s.Create(in); err == nil {
			t.Errorf("非法输入应报错: %+v", in)
		}
	}
}

// TestOpenClientKeysMissingVsCorrupt 文件不存在 = 未启用；文件坏了必须报错，
// 不能当成「没有密钥」（否则「建过密钥就必须带」会悄悄失效）。
func TestOpenClientKeysMissingVsCorrupt(t *testing.T) {
	s, err := OpenClientKeys(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || s == nil || s.HasKeys() {
		t.Fatalf("文件不存在应从空开始: err=%v", err)
	}

	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}
	if _, err := OpenClientKeys(path); err == nil {
		t.Fatal("坏文件必须报错")
	}
}
