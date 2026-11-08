package upstream

import (
	"encoding/json"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/prompt"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
)

const hotPathBody = `{"model":"cn:deepseek-v4.1-flash","conversationId":"c-1","stream":true,
	"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],
	"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`

// TestHotPathParseCount 守住「一个 chat 请求把同一个 body 整树解析几次」。
//
// 线上「并发一高就降速」的根因就是这里：同一个 100~500KB body 被 5~6 条互不
// 相干的代码路径各解析一遍（每条只读一两个字段）。解析次数不是审美问题——
// 170K token 的请求体单次整树解析约 30ms，10 请求/秒就是 0.3 个核。
//
// 用计数器而不是「返回值是不是同一块内存」来断言：EnsureSystemFirst 命中
// system 分支时本来就原样返回 body，指针相同并不代表没解析过（第一版测试就
// 因此漏检，变异测试没变红）。
func TestHotPathParseCount(t *testing.T) {
	body := []byte(hotPathBody)

	prompt.ResetParseCount()
	if out := prompt.EnsureSystemFirst(body, "fallback"); string(out) != hotPathBody {
		t.Fatalf("EnsureSystemFirst 改动了已是 system 首条的 body")
	}
	if n := prompt.ParseCount(); n != 0 {
		t.Errorf("EnsureSystemFirst 首条已是 system 时解析 %d 次，应为 0", n)
	}

	session.ResetParseCount()
	if got := session.ExtractKey(body); got != "c-1" {
		t.Errorf("ExtractKey = %q want c-1", got)
	}
	if got := session.ResolveConversationID(body); got != "c-1" {
		t.Errorf("ResolveConversationID = %q want c-1", got)
	}
	if n := session.ParseCount(); n != 0 {
		t.Errorf("显式会话键路径解析 %d 次，应为 0", n)
	}

	ResetKimiToolsParseCount()
	if out := prepareKimiTools(body, "cn"); string(out) != hotPathBody {
		t.Fatalf("prepareKimiTools 改动/重构了非 kimi 请求")
	}
	if n := KimiToolsParseCount(); n != 0 {
		t.Errorf("非 kimi 模型下 prepareKimiTools 解析 %d 次，应为 0", n)
	}

	// 归一化本身必须改结构，这一次解析无法避免；但会话字段注入必须搭它同一趟车。
	ResetPrepareBodyParseCount()
	out, _, _ := prepareBodyOpt(body, false, true, nil, nil, nil, "cn", nil, "uid", "c-1", true, 0, "conv-turn-1")
	if n := PrepareBodyParseCount(); n != 1 {
		t.Errorf("prepareBodyOpt 解析 %d 次，应为 1（会话字段注入已并入）", n)
	}
	var got struct {
		Messages []struct {
			Role                  string `json:"role"`
			ConversationRequestID string `json:"conversationRequestId"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) < 2 || got.Messages[1].ConversationRequestID != "conv-turn-1" {
		t.Fatalf("会话字段注入丢失: %s", out)
	}
}

// TestHotPathFallbacksStillParse 快路径的回落分支必须真的存在（扫描器认不出时
// 仍走整树解析，行为不退化）。
func TestHotPathFallbacksStillParse(t *testing.T) {
	weird := []byte(`not-json`)
	prompt.ResetParseCount()
	if out := prompt.EnsureSystemFirst(weird, "fallback"); string(out) != "not-json" {
		t.Fatalf("坏 body 应原样返回")
	}
	if n := prompt.ParseCount(); n != 1 {
		t.Errorf("坏 body 应回落整树解析 1 次，实际 %d", n)
	}

	kimi := []byte(`{"model":"cn:kimi-k2","tools":[]}`)
	ResetKimiToolsParseCount()
	prepareKimiTools(kimi, "cn")
	if n := KimiToolsParseCount(); n != 1 {
		t.Errorf("kimi 模型应解析 1 次，实际 %d", n)
	}
}
