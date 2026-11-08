package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
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
// 格式 = UUID v4 形态 36 字符（issue #51：官方 X-Conversation-ID 实测即此形态）；
// conversation_id 只驱动哈希段、不逐字出现（同 id 同 key、不同 id 不同 key）。
// 账号隔离改由 uid 参与哈希实现——键里不得再出现 uid 明文或项目缩写。
func TestInjectPromptCacheKey_UsesConversationID(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-42"}`

	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	assertUUIDv4(t, got)
	assertNoFingerprint(t, got, uid)
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
	assertUUIDv4(t, ka)
	assertUUIDv4(t, kb)
}

// TestInjectPromptCacheKey_Format 覆盖任务用例 5：
// cache key 形态校验——必须与官方 X-Conversation-ID 同形（UUID v4 36 字符）。
func TestInjectPromptCacheKey_Format(t *testing.T) {
	uid := "1234567890abcdef"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "conv-fmt")
	got := extractCacheKey(t, out)
	assertUUIDv4(t, got)
	// issue #51：uid 明文段曾经是「账号隔离」的实现方式，现在隔离靠哈希输入，
	// 明文不得出现在出站键里（否则等于把账号 ID 打给上游）。
	assertNoFingerprint(t, got, uid)
}

// TestInjectPromptCacheKey_EmptyConversation 覆盖任务用例 3 边界：
// 无 conversation_id 且无会话标识 → 仍生成非空、形态合法的键（哈希源固化为空串），
// 不应报错、不应空串。
func TestInjectPromptCacheKey_EmptyConversation(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if got == "" {
		t.Fatalf("expected non-empty key when no conversation, got empty")
	}
	assertUUIDv4(t, got)
	assertNoFingerprint(t, got, uid)
}

// TestChatMetaCacheKeySource 无 conversation_id 的客户端：缓存槽（prompt_cache_key）与
// X-Conversation-ID 头**同源**，都跟网关派生的粘性会话键（CacheKeyID）——实测该头才是上游
// 前缀缓存槽的实际来源，不发它时大前缀在 hy4/step 这类后端上恒 0% 命中（issue #169）。
// 客户端自带 conversation_id 时两者仍以客户端声明为准（原值透传，不覆盖）。
func TestChatMetaCacheKeySource(t *testing.T) {
	for _, tc := range []struct {
		name      string
		meta      ChatMeta
		wantKey   string
		wantConvH string
		// wantConvAny：两源皆空时该头**不再留空**（上游渠道校验要求三件套齐全，缺任一头
		// 即 400 code=11-128 "conversation id missing"）——现造 32 hex，值不可预知，只断言非空。
		wantConvAny bool
	}{
		// X-Conversation-ID 派生分支的形态是 UUIDv7（真机恒 01a1815d-…），与
		// prompt_cache_key 的 v4 槽键不同函数、不同值（2026-10-28 形态修正）。
		{"仅粘性键", ChatMeta{CacheKeyID: "sess-abc"}, buildCacheKey("u1", "sess-abc"), buildTimeOrderedUUID("u1", "sess-abc", ""), false},
		{"客户端会话优先", ChatMeta{ConversationID: "conv-1", CacheKeyID: "sess-abc"}, buildCacheKey("u1", "conv-1"), "conv-1", false},
		{"两源皆空", ChatMeta{}, buildCacheKey("u1", ""), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			var gotConv string
			c := testClient(func(r *http.Request) (*http.Response, error) {
				gotBody, _ = io.ReadAll(r.Body)
				// headerFold 而非 Get：出站 key 已是官方拼写 X-Conversation-ID
				gotConv = headerFold(r.Header, "X-Conversation-ID")
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
			if tc.wantConvAny {
				if gotConv == "" {
					t.Error("X-Conversation-ID 不得留空：上游渠道校验要求三件套齐全")
				}
			} else if gotConv != tc.wantConvH {
				t.Errorf("X-Conversation-ID=%q want %q（无客户端声明时用 uid 隔离的会话键）", gotConv, tc.wantConvH)
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

func TestPromptCacheKeyPrecedence(t *testing.T) {
	const uid = "user-abc-123"
	uidKey := func(conv string) string { return buildCacheKey(uid, conv) }

	cases := []struct {
		name string
		obj  map[string]any
		conv string
		want string // 空串表示不应注入（ok=false）
	}{
		{"existing client key", map[string]any{"prompt_cache_key": "client-key"}, "arg-conv", ""},
		{"empty client key", map[string]any{"prompt_cache_key": ""}, "arg-conv", uidKey("arg-conv")},
		{"non-string client key", map[string]any{"prompt_cache_key": 123}, "arg-conv", uidKey("arg-conv")},
		{"snake conversation", map[string]any{"conversation_id": "conv-42"}, "arg-conv", uidKey("conv-42")},
		{"camel conversation", map[string]any{"conversationId": "conv-43"}, "arg-conv", uidKey("conv-43")},
		{"snake wins over camel", map[string]any{"conversation_id": "snake", "conversationId": "camel"}, "arg-conv", uidKey("snake")},
		{"fallback to arg", map[string]any{}, "arg-conv", uidKey("arg-conv")},
		{"all empty", map[string]any{}, "", uidKey("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := promptCacheKeyFor(c.obj, uid, c.conv)
			if c.want == "" {
				if ok {
					t.Fatalf("ok=true key=%q, want no injection", got)
				}
				return
			}
			if !ok || got != c.want {
				t.Fatalf("got=(%q,%v), want (%q,true)", got, ok, c.want)
			}
		})
	}
}

// TestPrepareBodyCacheKeyEquivalence 钉住「同一次解析注入」与旧「两遍
// PrepareBodyOpt + InjectPromptCacheKey」的最终字节完全一致。
func TestPrepareBodyCacheKeyEquivalence(t *testing.T) {
	const uid = "user-abc-123"
	const conv = "conv-arg"
	bodies := []string{
		`{"model":"glm-5.2","messages":[]}`,
		`{"model":"glm-5.2","messages":[],"prompt_cache_key":"client-key"}`,
		`{"model":"glm-5.2","messages":[],"conversation_id":"conv-42"}`,
		`{"model":"glm-5.2","messages":[],"conversationId":"conv-43"}`,
		`{"model":"glm-5.2","messages":[],"conversation_id":"snake","conversationId":"camel"}`,
		`{"model":"glm-5.2","messages":[],"prompt_cache_key":123}`,
		`not json`,
		``,
	}
	for _, in := range bodies {
		t.Run(in, func(t *testing.T) {
			oldBody := InjectPromptCacheKey(PrepareBodyOpt([]byte(in), false), uid, conv)
			newBody, _, _ := prepareBodyOpt([]byte(in), false, true, nil, nil, nil, "", nil, uid, conv, true, 0, "")
			if !bytes.Equal(oldBody, newBody) {
				t.Fatalf("body mismatch\nold=%s\nnew=%s", oldBody, newBody)
			}
		})
	}
}

// uuidV4Re 官方出站标识形态：36 字符带横线 UUID，version 位 4、variant 位 8/9/a/b。
// 依据：国际版 5.8.4 真机抓包 X-Conversation-ID = 9c0dca10-1110-4bd0-9bc0-9dc9f3440631
// （capture/REPORT.md 2.1）。
var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// assertUUIDv4 断言标识与官方同形：长度 36、带横线、version=4。
// 长度/字符集/前缀任一处不同，上游都可据此把网关流量单独圈出来（issue #51）。
func assertUUIDv4(t *testing.T, got string) {
	t.Helper()
	if !uuidV4Re.MatchString(got) {
		t.Fatalf("want UUID v4 shape (official X-Conversation-ID form), got=%q (len=%d)", got, len(got))
	}
}

// assertNoFingerprint 断言出站标识里不含项目自证串与账号明文。
func assertNoFingerprint(t *testing.T, got, uid string) {
	t.Helper()
	for _, bad := range []string{"wb2a", "wb2api", "workbuddy2api"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Fatalf("outbound identifier leaks project marker %q: %s", bad, got)
		}
	}
	if uid != "" {
		head := uid
		if len(head) > 8 {
			head = head[:8]
		}
		if strings.Contains(got, head) {
			t.Fatalf("outbound identifier leaks uid prefix %q: %s", head, got)
		}
	}
}

// TestMessageIDIsTimeOrdered 钉住「消息级 ID 带毫秒时间位」这一官方形态（issue #51）：
// 官方 X-Request-ID 前 48 bit 是毫秒时间戳、同会话内严格单调；旧实现纯随机，
// 按时间排序是乱的，构成可聚类的批次特征。
func TestMessageIDIsTimeOrdered(t *testing.T) {
	first := session.NewMessageID()
	second := session.NewMessageID()
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("message id must stay 32 hex (B3 TraceId 合法集): %q %q", first, second)
	}
	if !isHex(first) || !isHex(second) {
		t.Fatalf("message id must be hex: %q %q", first, second)
	}
	var ms1, ms2 int64
	if _, err := fmt.Sscanf(first[:12], "%x", &ms1); err != nil {
		t.Fatalf("parse time prefix: %v", err)
	}
	if _, err := fmt.Sscanf(second[:12], "%x", &ms2); err != nil {
		t.Fatalf("parse time prefix: %v", err)
	}
	now := time.Now().UnixMilli()
	if d := now - ms1; d < -3600_000 || d > 3600_000 {
		t.Fatalf("time prefix not close to now: prefix=%d now=%d", ms1, now)
	}
	if ms2 < ms1 {
		t.Fatalf("message ids not monotonic: %d > %d", ms1, ms2)
	}
}

// TestAggregationIDsStayDeterministic 钉住一个**故意的取舍**：TurnRequestID /
// RequestIDForKey 不带时间位。它们必须同键同值，否则一次 user send 的多条上游请求
// （tool call 多轮 / 换号重试 / 降级重发）会各拿一个 ID，issue #35 的聚合重新打散。
func TestAggregationIDsStayDeterministic(t *testing.T) {
	const turn = "u3:hello world"
	if a, b := session.TurnRequestID(turn), session.TurnRequestID(turn); a != b {
		t.Fatalf("TurnRequestID must be deterministic for the same turn key: %q vs %q", a, b)
	}
	if a, b := session.RequestIDForKey("sess-1"), session.RequestIDForKey("sess-1"); a != b {
		t.Fatalf("RequestIDForKey must be deterministic for the same key: %q vs %q", a, b)
	}
	if session.TurnRequestID("u3:hello") == session.TurnRequestID("u3:bye") {
		t.Fatal("different turn keys must not collide")
	}
}

// isHex 供上面的形态断言使用。
func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
