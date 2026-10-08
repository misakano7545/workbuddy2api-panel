package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// extractCacheKey 从改写后的 body 里取出 prompt_cache_key 字段值。
func extractCacheKey(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	v, _ := obj["prompt_cache_key"].(string)
	return v
}

// TestInjectPromptCacheKey_PreservesExisting 覆盖任务用例 1：
// 入站 body 已带 prompt_cache_key → 原值保留，不被网关覆盖。
func TestInjectPromptCacheKey_PreservesExisting(t *testing.T) {
	// Arrange
	uid := "user-abc-123"
	conv := "conv-xyz"
	in := `{"model":"glm-5.2","messages":[],"prompt_cache_key":"client-set-key"}`

	// Act
	out := InjectPromptCacheKey([]byte(in), uid, conv)

	// Assert
	if got := extractCacheKey(t, out); got != "client-set-key" {
		t.Fatalf("existing prompt_cache_key overwritten: got=%q want client-set-key", got)
	}
}

// TestInjectPromptCacheKey_UsesConversationID 覆盖任务用例 2：
// body 无 prompt_cache_key 但有 conversation_id → 用它做会话哈希源。
// 格式 wb2a-<uid8>-<conversationHash>：conversation_id 不逐字出现，而是驱动哈希段
// （同 id 同 key、不同 id 不同 key）。同时仍带 uid8 隔离前缀。
func TestInjectPromptCacheKey_UsesConversationID(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-42"}`

	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "user-abc") {
		t.Fatalf("expected uid8 isolation prefix, got=%q", got)
	}
	// 同 conversation_id 两次出站 → 同 key（稳定）。
	out2 := InjectPromptCacheKey([]byte(in), uid, "")
	if got2 := extractCacheKey(t, out2); got2 != got {
		t.Fatalf("non-deterministic key for same conversation_id: %q vs %q", got, got2)
	}
	// 不同 conversation_id → 不同 key（哈希段区分）。
	inB := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-99"}`
	gotB := extractCacheKey(t, InjectPromptCacheKey([]byte(inB), uid, ""))
	if gotB == got {
		t.Fatalf("different conversation_id produced same key: %q", got)
	}
}

// TestInjectPromptCacheKey_GeneratesStableKey 覆盖任务用例 3：
// body 两者都无 → 网关生成稳定键，两次同输入同账号得到同 key。
func TestInjectPromptCacheKey_GeneratesStableKey(t *testing.T) {
	uid := "user-abc-123"
	conv := "conv-stable"
	in := `{"model":"glm-5.2","messages":[]}`

	a := InjectPromptCacheKey([]byte(in), uid, conv)
	b := InjectPromptCacheKey([]byte(in), uid, conv)
	ka, kb := extractCacheKey(t, a), extractCacheKey(t, b)
	if ka != kb {
		t.Fatalf("non-deterministic key for same input+uid: a=%q b=%q", ka, kb)
	}
}

// TestInjectPromptCacheKey_AccountIsolation 覆盖任务用例 4：
// 不同账号 → key 不同（隔离验证）。
func TestInjectPromptCacheKey_AccountIsolation(t *testing.T) {
	conv := "conv-shared"
	in := `{"model":"glm-5.2","messages":[]}`

	ka := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "user-aaa-111", conv))
	kb := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "user-bbb-222", conv))
	if ka == kb {
		t.Fatalf("cross-account key collision: both=%q", ka)
	}
	if !strings.HasPrefix(ka, "wb2a-") || !strings.HasPrefix(kb, "wb2a-") {
		t.Fatalf("keys must carry wb2a- prefix: a=%q b=%q", ka, kb)
	}
}

// TestInjectPromptCacheKey_Format 覆盖任务用例 5：
// cache key 格式校验（含 uid8 前缀）。
func TestInjectPromptCacheKey_Format(t *testing.T) {
	uid := "1234567890abcdef"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "conv-fmt")
	got := extractCacheKey(t, out)
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "12345678") {
		t.Fatalf("expected uid8 (12345678) in key, got=%q", got)
	}
}

// TestInjectPromptCacheKey_EmptyConversation 覆盖任务用例 3 边界：
// 无 conversation_id 且无会话标识 → 生成键含 uid8 但 conversationHash 段为定值（不复用前缀）。
// 不应报错、不应空串（key 非空）。
func TestInjectPromptCacheKey_EmptyConversation(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if got == "" {
		t.Fatalf("expected non-empty key when no conversation, got empty")
	}
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "user-abc") {
		t.Fatalf("expected uid8 in key, got=%q", got)
	}
}

// TestChatMetaCacheKeySource 缓存槽来源与头部来源分开：无 conversation_id 时缓存槽跟网关
// 派生的粘性会话键（CacheKeyID），但 X-Conversation-ID 仍然不发（不伪造）；客户端自带
// conversation_id 时缓存槽仍以客户端声明为准。
func TestChatMetaCacheKeySource(t *testing.T) {
	for _, tc := range []struct {
		name      string
		meta      ChatMeta
		wantKey   string
		wantConvH string
	}{
		{"仅粘性键", ChatMeta{CacheKeyID: "sess-abc"}, buildCacheKey("u1", "sess-abc"), ""},
		{"客户端会话优先", ChatMeta{ConversationID: "conv-1", CacheKeyID: "sess-abc"}, buildCacheKey("u1", "conv-1"), "conv-1"},
		{"两源皆空", ChatMeta{}, buildCacheKey("u1", ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			var gotConv string
			c := testClient(func(r *http.Request) (*http.Response, error) {
				gotBody, _ = io.ReadAll(r.Body)
				gotConv = r.Header.Get("X-Conversation-ID")
				return &http.Response{StatusCode: 200,
					Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:   io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
			})
			a := &auth.Auth{AccessToken: "at", UID: "u1"}
			rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", tc.meta)
			if err != nil || status != 200 {
				t.Fatalf("status=%d err=%v", status, err)
			}
			rc.Close()
			if got := extractCacheKey(t, gotBody); got != tc.wantKey {
				t.Errorf("prompt_cache_key=%q want %q", got, tc.wantKey)
			}
			if gotConv != tc.wantConvH {
				t.Errorf("X-Conversation-ID=%q want %q（客户端没给就不该伪造）", gotConv, tc.wantConvH)
			}
		})
	}
}

// TestCacheSlotFromStickySessionKey 无 conversation_id 的客户端：缓存槽按网关派生的
// 粘性会话键分家 —— 同会话跨轮同槽（历史每轮追加也不换），异会话异槽。
//
// 为什么这条链必须钉住：粘性键 session.ExtractKey 是「会话级」（conv id / 客户端
// prompt_cache_key / system+首条 user 的哈希），而轮级键 TurnKey 每轮都变。若哪天把
// 轮级键接进缓存槽，每个对话轮都会换一个新 key → 前缀缓存永远冷启动（费用反向）。
// 下方 TurnKey 断言即本用例的敏感度自证：两个轮级键确实不同。
func TestCacheSlotFromStickySessionKey(t *testing.T) {
	body := func(firstUser, second string) []byte {
		msgs := `{"role":"system","content":"you are X"},{"role":"user","content":"` + firstUser + `"}`
		if second != "" {
			msgs += `,{"role":"assistant","content":"ok"},{"role":"user","content":"` + second + `"}`
		}
		return []byte(`{"model":"m","messages":[` + msgs + `]}`)
	}
	slotFor := func(b []byte) string {
		t.Helper()
		sk := session.ExtractKey(b)
		if sk == "" {
			t.Fatalf("ExtractKey 应派生出非空粘性键: %s", b)
		}
		return buildCacheKey("u1", cacheKeySource(ChatMeta{CacheKeyID: sk}))
	}
	turn1, turn2, other := body("hello", ""), body("hello", "next"), body("another topic", "")
	if a, b := slotFor(turn1), slotFor(turn2); a != b {
		t.Fatalf("同会话跨轮换了缓存槽：%q → %q", a, b)
	}
	if slotFor(turn1) == slotFor(other) {
		t.Fatalf("异会话共用一个缓存槽：%q", slotFor(turn1))
	}
	if session.TurnKey(turn1) == session.TurnKey(turn2) {
		t.Fatal("敏感度自证失败：轮级键本该逐轮不同")
	}
}

// TestPrepareBodyOptNoCacheKeyInjection 覆盖任务用例 6：
// PrepareBodyOpt（sanitize=false 的旧入口）行为不变——不注入 cache key。
// 向后兼容：仅传 body 不给 cacheKey 上下文时，不得引入新字段。
func TestPrepareBodyOptNoCacheKeyInjection(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[]}`
	out := PrepareBodyOpt([]byte(in), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := obj["prompt_cache_key"]; present {
		t.Fatalf("PrepareBodyOpt must NOT inject prompt_cache_key, but got: %v", obj["prompt_cache_key"])
	}
}
