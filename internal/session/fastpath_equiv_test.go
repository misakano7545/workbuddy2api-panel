package session

import (
	"encoding/json"
	"testing"
)

// refExtractKey 是快路径出现之前的原实现（整树解析），作为等价性基准保留在测试里。
func refExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	if hasUserIDKey(obj) {
		return ""
	}
	return deriveKey(obj)
}

// refResolveConversationID 同上，整树版本的 ResolveConversationID。
func refResolveConversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	return strOrEmpty(obj["conversationId"])
}

// TestExtractKeyFastPathEquivalence 快路径必须与整树实现给出完全相同的结果。
//
// 这是本类优化的核心风险：会话键决定账号粘性与前缀缓存槽，判错会让同一对话
// 在两个账号间来回跳（缓存连续 miss、上游计费翻倍）。所以逐例对拍，不靠推理。
func TestExtractKeyFastPathEquivalence(t *testing.T) {
	bodies := []string{
		// 显式键：4 种命名 + 优先级
		`{"metadata":{"conversation_id":"m-snake","conversationId":"m-camel"},"conversation_id":"top-snake","conversationId":"top-camel","prompt_cache_key":"pc","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`,
		`{"metadata":{"conversationId":"m-camel"},"conversation_id":"top-snake","messages":[]}`,
		`{"conversation_id":"top-snake","conversationId":"top-camel","messages":[]}`,
		`{"conversationId":"top-camel","prompt_cache_key":"pc","messages":[]}`,
		`{"prompt_cache_key":"pc","messages":[]}`,
		// 空串 / 非字符串：都应视为未命中
		`{"conversation_id":"","metadata":{"conversation_id":""},"messages":[]}`,
		`{"conversation_id":123,"metadata":{"conversationId":true},"messages":[]}`,
		`{"metadata":"not-an-object","conversationId":"c","messages":[]}`,
		// user_id 闸
		`{"conversation_id":"","metadata":{"user_id":"u1"},"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`,
		`{"user_id":"u1","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`,
		// 派生键
		`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"first"}]}`,
		`{"messages":[{"role":"user","content":"only-user"}]}`,
		`{"messages":[]}`,
		`{"messages":"nope"}`,
		`{}`,
		``,
		// 非对象 / 坏 JSON
		`[]`,
		`null`,
		`{"conversation_id":"unterminated`,
		// 转义值
		`{"conversation_id":"a\"b\\c","messages":[]}`,
		`{"metadata":{"conversation_id":"\u4e2d\u6587-id"},"messages":[]}`,
		// 深层嵌套后再出现显式键（扫描器要能跳过）
		`{"tools":[{"function":{"parameters":{"$defs":{"a":{"b":[1,2,{"c":"}"}]}}}}}],"conversationId":"after-deep","messages":[]}`,
	}
	for _, b := range bodies {
		want := refExtractKey([]byte(b))
		if got := ExtractKey([]byte(b)); got != want {
			t.Errorf("ExtractKey(%s) = %q want %q", b, got, want)
		}
		wantCID := refResolveConversationID([]byte(b))
		if got := ResolveConversationID([]byte(b)); got != wantCID {
			t.Errorf("ResolveConversationID(%s) = %q want %q", b, got, wantCID)
		}
	}
}

// TestExtractKeyFastPathLargeBody 大 body 上的等价性（快路径的主要收益场景）。
func TestExtractKeyFastPathLargeBody(t *testing.T) {
	block := make([]byte, 0, 200*1024)
	for i := 0; i < 4000; i++ {
		block = append(block, []byte(`{"role":"assistant","content":"some long agent history chunk with \"quotes\" and \n newlines"}`)...)
		if i < 3999 {
			block = append(block, ',')
		}
	}
	bodies := []string{
		`{"conversationId":"big-1","messages":[` + string(block) + `]}`,
		`{"messages":[` + string(block) + `]}`,
	}
	for i, b := range bodies {
		want := refExtractKey([]byte(b))
		if got := ExtractKey([]byte(b)); got != want {
			t.Errorf("case %d: got %q want %q", i, got, want)
		}
	}
}

// TestResolveConversationIDNeverFallsBackToUserID 契约：只认 conversationId。
func TestResolveConversationIDNeverFallsBackToUserID(t *testing.T) {
	body := []byte(`{"user_id":"u-1","metadata":{"user_id":"u-1"},"messages":[]}`)
	if got := ResolveConversationID(body); got != "" {
		t.Fatalf("不应回落 user_id，得到 %q", got)
	}
}

func BenchmarkExtractKeyExplicitLarge(b *testing.B) {
	block := make([]byte, 0, 400*1024)
	for i := 0; i < 8000; i++ {
		block = append(block, []byte(`{"role":"assistant","content":"agent history chunk 0123456789 abcdefghij"}`)...)
		if i < 7999 {
			block = append(block, ',')
		}
	}
	body := []byte(`{"conversationId":"bench-1","messages":[` + string(block) + `]}`)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ExtractKey(body) != "bench-1" {
			b.Fatal("wrong key")
		}
	}
}

func BenchmarkExtractKeyRefLarge(b *testing.B) {
	block := make([]byte, 0, 400*1024)
	for i := 0; i < 8000; i++ {
		block = append(block, []byte(`{"role":"assistant","content":"agent history chunk 0123456789 abcdefghij"}`)...)
		if i < 7999 {
			block = append(block, ',')
		}
	}
	body := []byte(`{"conversationId":"bench-1","messages":[` + string(block) + `]}`)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if refExtractKey(body) != "bench-1" {
			b.Fatal("wrong key")
		}
	}
}
