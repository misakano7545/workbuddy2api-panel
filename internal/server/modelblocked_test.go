package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// quotaLockBody 上游 6004「使用量已超出频率限制」的真实形态（带重置墙钟）。
const quotaLockBody = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-25 23:00:00 重置","requestId":"rq-quota"}`

// TestChatAllAccountsRuleBlockedReturns400 本域所有可选账号都被模型规则挡住某个模型时回
// 400 model_not_allowed：规则不会自愈，落 503 no_healthy_account 会被客户端当成上游抖动
// 反复重试（与上面 6004 配额锁同一个理由）。
func TestChatAllAccountsRuleBlockedReturns400(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "r1", AccessToken: "at-r1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "r2", AccessToken: "at-r2", ExpiresAt: 9999999999},
	)
	rule, err := p.UpsertRule(pool.ModelRule{Name: "贵模型不给这两个号", Mode: pool.RuleModeDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	if _, err := p.ApplyRule(rule.ID, []string{"r1", "r2"}, true); err != nil {
		t.Fatalf("ApplyRule: %v", err)
	}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("全池被规则挡住时不应打上游，auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400 model_not_allowed)", rec.Code, rec.Body)
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
			Hint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v body=%s", err, rec.Body)
	}
	if resp.Error.Code != "model_not_allowed" {
		t.Fatalf("error.code=%q want model_not_allowed (body=%s)", resp.Error.Code, rec.Body)
	}
	// hint 必须点明是规则干的：503 那条路上看不出成因，运维排查「明明有号却失败」全靠这句。
	if !strings.Contains(resp.Error.Hint, "model_rules: 2/2") {
		t.Errorf("gateway_hint=%q 应带 blocked/eligible 计数", resp.Error.Hint)
	}
}

// TestChatModelQuotaLockedReturns429 全池该模型都被 6004 用量配额锁住时，回 429 +
// 上游业务码 + 上游原文，且**不带 Retry-After**：重置前重试必然同样失败，客户端
// 该立刻报错（换模型/等额度），而不是按 Retry-After 静默睡到解锁（codebuddy CLI
// 直接 delayMs = retry-after×1000，20 分钟的冷却就变成 20 分钟挂起）。
func TestChatModelQuotaLockedReturns429(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "q1", AccessToken: "at-q1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "q2", AccessToken: "at-q2", ExpiresAt: 9999999999},
	)
	reset := time.Now().Add(20 * time.Minute)
	for _, uid := range []string{"q1", "q2"} {
		p.CooldownSoftForModel(uid, time.Minute, reset, "glm-5.2", quotaLockBody)
	}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("全池配额锁时不应打上游，auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 429 {
		t.Fatalf("code=%d body=%s (want 429)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("配额锁不该发 Retry-After（客户端会照睡到解锁），got %q", got)
	}
	var resp struct {
		Error struct {
			Code    int    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v body=%s", err, rec.Body)
	}
	if resp.Error.Code != 6004 {
		t.Errorf("error.code=%d want 6004（数字业务码，客户端据此判不可重试）", resp.Error.Code)
	}
	if resp.Error.Type != "insufficient_quota" {
		t.Errorf("error.type=%q want insufficient_quota", resp.Error.Type)
	}
	if !strings.Contains(resp.Error.Message, "您的使用量已超出频率限制") {
		t.Errorf("message 应透传上游原文，got %q", resp.Error.Message)
	}
	if strings.Contains(rec.Body.String(), "model_busy") {
		t.Errorf("配额锁不该报成 model_busy（那是可重试语义）：%s", rec.Body)
	}
}

// TestChatModelQuotaMixedWithBusyKeeps503 只有**全池同因**（都是配额）才算配额锁：
// 混进 14003（瞬时繁忙，等就有）时仍走可重试的 503——不能因为有一个号配额耗尽，
// 就把"再等一分钟就能用"判成"别重试"。
func TestChatModelQuotaMixedWithBusyKeeps503(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "q1", AccessToken: "at-q1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "q2", AccessToken: "at-q2", ExpiresAt: 9999999999},
	)
	p.CooldownSoftForModel("q1", time.Minute, time.Now().Add(20*time.Minute), "glm-5.2", quotaLockBody)
	p.CooldownSoftForModel("q2", time.Minute, time.Now().Add(time.Minute), "glm-5.2", "14003 model busy")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("模型级阻塞时不应打上游，auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503 model_busy)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_busy") {
		t.Errorf("混锁时不该按配额 429 报：%s", rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("可重试的 503 应带 Retry-After")
	}
}

// TestChatModelBusyRetryAfterCapped 14003（瞬时繁忙）仍走 503 + Retry-After，但头上限
// 600s：CLI 把该头当 delayMs 直接睡，冷却很长时不能把真实剩余时间原样报出去。
func TestChatModelBusyRetryAfterCapped(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "b1", AccessToken: "at-b1", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("b1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "14003 model busy")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("模型繁忙冷却时不应打上游，auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503 model_busy)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "600" {
		t.Errorf("Retry-After=%q want 600（封顶 10 分钟）", got)
	}
	if !strings.Contains(rec.Body.String(), "model_busy") {
		t.Errorf("应保持 model_busy：%s", rec.Body)
	}
}

// TestChatModelBlockedReportsModelUnavailable 池里号都在、只是每个号都对该模型处于
// 模型级冷却（11102/6004）时，客户端必须看到 400 model_unavailable + 本地调度信息，
// 而不是「没有可用账号」的 503——前者该换模型，后者该等，处置相反（吸收上游 44d6e04）。
func TestChatModelBlockedReportsModelUnavailable(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	p.BlockModelBackoff("u1", "glm-5.2", "11102 model [glm-5.2] service info not found")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("模型全池冷却时不应打上游（选号就该失败），auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400 model_unavailable)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "model_unavailable") {
		t.Errorf("body 应带 code=model_unavailable：%s", body)
	}
	if !strings.Contains(body, "model_blocked: 1 account") {
		t.Errorf("gateway_hint 应带被挡账号数：%s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("不得退回 no_healthy_account 文案：%s", body)
	}
}

// TestChatNoAccountsStillNoHealthyAccount 池子真没号（无模型冷却）时口径不变：
// 仍是 503 + no_healthy_account，绝不能冒充模型不可用（防止 ModelBlocked 误报）。
func TestChatNoAccountsStillNoHealthyAccount(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	p.Disable("u1", "test") // 唯一账号被禁用 → 真没号
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("无可用账号时不应打上游，auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503 no_healthy_account)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no_healthy_account") {
		t.Errorf("body 应保持 no_healthy_account：%s", body)
	}
	if strings.Contains(body, "model_unavailable") {
		t.Errorf("真没号不得报成模型不可用：%s", body)
	}
}

// TestApplyErrorPolicyModelForbiddenUses11158Reason 11158「该账号类型无此模型权限」
// 走的是同一个 (账号,模型) 负缓存通道，但 reason 必须记 11158——运维在 /status
// 台账上要能分清「这个模型在后端不存在」与「这个号没这个模型的权限」，后者换号就能解。
func TestApplyErrorPolicyModelForbiddenUses11158Reason(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	h := NewHandler(Config{Pool: p})

	body := `{"code":11158,"msg":"model [claude-opus-4-1] is not allowed for user type","requestId":"r1"}`
	if kind := upstream.Classify(400, body); kind != upstream.ErrModelBlocked {
		t.Fatalf("Classify=%v want ErrModelBlocked", kind)
	}
	h.applyErrorPolicy("u1", upstream.ErrModelBlocked, body, "claude-opus-4-1", nil)

	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("模型级权限拒绝不得禁用账号：%+v", st)
	}
	// 模型级拒绝只避让该模型，账号仍可服务别的模型——不得写账号级冷却。
	if st.Cooling {
		t.Fatalf("11158 是模型级拒绝，不该把整个账号冷却掉（kind=%q）", st.CoolKind)
	}
	var got string
	for _, row := range st.RateLimitedModels {
		if row.Model == "claude-opus-4-1" {
			got = row.Reason
			if row.Kind != "model_unavailable" {
				t.Errorf("台账 kind=%q want model_unavailable", row.Kind)
			}
		}
	}
	if !strings.HasPrefix(got, upstream.ModelForbiddenReason) {
		t.Fatalf("台账 reason=%q want 前缀 %q", got, upstream.ModelForbiddenReason)
	}
}
