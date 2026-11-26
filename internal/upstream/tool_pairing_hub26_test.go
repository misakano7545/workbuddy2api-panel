package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// hub #26 同款三修的回归位（workbuddy2api-hub 2026-11-24/26 三个提交）：
//
//  1. mergeAdjacentToolCalls —— 相邻 assistant.tool_calls 批（中间夹正文）在 wire 上
//     是同一轮，拆开会让前一批变成「无结果调用」，上游判 11148；
//  2. repackToolResultBlocks 不被孤儿结果 break —— 孤儿后面还有本批合法结果时，
//     break 会让那批结果带着插入物原样出站，照样 11148；
//  3. cleanupOrphanToolCalls 去重 —— 同一 call_id 的重复结果只留第一条。
//
// 全部走完整出站管线断言（prepareBodyOpt 内的调用顺序：merge → reorder → repack → cleanup）。
func TestToolPairingHub26Repairs(t *testing.T) {
	run := func(t *testing.T, msgs string) []map[string]any {
		t.Helper()
		const callJSON = `{"id":"IDX","type":"function","function":{"name":"f","arguments":"{}"}}`
		msgs = strings.ReplaceAll(msgs, "CALL0", strings.ReplaceAll(callJSON, "IDX", "c0"))
		msgs = strings.ReplaceAll(msgs, "CALL1", strings.ReplaceAll(callJSON, "IDX", "c1"))
		out := PrepareBodyOpt([]byte(`{"model":"m","messages":`+msgs+`}`), false)
		var obj struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("出站不是合法 body: %v\n%s", err, out)
		}
		return obj.Messages
	}
	shape := func(msgs []map[string]any) string {
		var parts []string
		for _, m := range msgs {
			role, _ := m["role"].(string)
			if tcs, ok := m["tool_calls"].([]any); ok {
				var ids []string
				for _, tci := range tcs {
					tc, _ := tci.(map[string]any)
					id, _ := tc["id"].(string)
					ids = append(ids, id)
				}
				parts = append(parts, role+".calls("+strings.Join(ids, ",")+")")
				continue
			}
			if role == "tool" {
				id, _ := m["tool_call_id"].(string)
				parts = append(parts, "tool("+id+")")
				continue
			}
			parts = append(parts, role)
		}
		return strings.Join(parts, " | ")
	}

	call := func(id string) map[string]any {
		return map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": "f", "arguments": "{}"}}
	}

	t.Run("相邻两批并成一批：正文并入前一批的 assistant", func(t *testing.T) {
		got := run(t, `[
			{"role":"user","content":"go"},
			{"role":"assistant","content":"","tool_calls":[CALL0]},
			{"role":"assistant","content":"thinking out loud","tool_calls":[CALL1]},
			{"role":"tool","tool_call_id":"c0","content":"r0"},
			{"role":"tool","tool_call_id":"c1","content":"r1"}]`)
		if s := shape(got); s != `user | assistant.calls(c0,c1) | tool(c0) | tool(c1)` {
			t.Fatalf("shape = %s", s)
		}
		merged := got[1]
		if c, _ := merged["content"].(string); c != "thinking out loud" {
			t.Errorf("正文丢了：%q", c)
		}
		if rc, _ := merged["reasoning_content"].(string); rc != "" {
			t.Errorf("不该有 reasoning：%q", rc)
		}
	})

	t.Run("调用与结果之间的纯正文 assistant 并回调用批（插入物判配对断裂）", func(t *testing.T) {
		got := run(t, `[
			{"role":"user","content":"go"},
			{"role":"assistant","content":"","tool_calls":[CALL0]},
			{"role":"assistant","content":"mid"},
			{"role":"tool","tool_call_id":"c0","content":"r0"}]`)
		if s := shape(got); s != `user | assistant.calls(c0) | tool(c0)` {
			t.Fatalf("shape = %s", s)
		}
		if c, _ := got[1]["content"].(string); c != "mid" {
			t.Errorf("正文丢了：%q", c)
		}
	})

	t.Run("纯文本相邻回复不合并", func(t *testing.T) {
		got := run(t, `[
			{"role":"user","content":"go"},
			{"role":"assistant","content":"a"},
			{"role":"assistant","content":"b"}]`)
		if s := shape(got); s != `user | assistant | assistant` {
			t.Fatalf("shape = %s", s)
		}
	})

	t.Run("孤儿结果不遮挡本批后续结果：break 修复", func(t *testing.T) {
		got := run(t, `[
			{"role":"user","content":"go"},
			{"role":"assistant","content":"","tool_calls":[CALL0,CALL1]},
			{"role":"tool","tool_call_id":"c0","content":"r0"},
			{"role":"tool","tool_call_id":"ghost","content":"x"},
			{"role":"user","content":"<image_resize_notice>"},
			{"role":"tool","tool_call_id":"c1","content":"r1"}]`)
		if s := shape(got); s != `user | assistant.calls(c0,c1) | tool(c0) | tool(c1) | user` {
			t.Fatalf("shape = %s", s)
		}
	})

	t.Run("同一 call_id 的重复结果只留第一条", func(t *testing.T) {
		got := run(t, `[
			{"role":"user","content":"go"},
			{"role":"assistant","content":"","tool_calls":[CALL0]},
			{"role":"tool","tool_call_id":"c0","content":"r0"},
			{"role":"tool","tool_call_id":"c0","content":"r0-dup"}]`)
		if s := shape(got); s != `user | assistant.calls(c0) | tool(c0)` {
			t.Fatalf("shape = %s", s)
		}
		if c, _ := got[2]["content"].(string); c != "r0" {
			t.Errorf("应保留第一条结果，实得 %q", c)
		}
	})

	t.Run("工具结果两轮交错的健康流量零改动", func(t *testing.T) {
		got := run(t, `[
			{"role":"user","content":"go"},
			{"role":"assistant","content":"","tool_calls":[CALL0]},
			{"role":"tool","tool_call_id":"c0","content":"r0"},
			{"role":"assistant","content":"","tool_calls":[CALL1]},
			{"role":"tool","tool_call_id":"c1","content":"r1"}]`)
		if s := shape(got); s != `user | assistant.calls(c0) | tool(c0) | assistant.calls(c1) | tool(c1)` {
			t.Fatalf("shape = %s", s)
		}
	})

	// 单函数层面再钉两条：merge 对结构化 parts 不动手；dedupe 标记 changed。
	t.Run("merge 结构化 parts 原样跳过", func(t *testing.T) {
		in := []any{
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call("c0")}},
			map[string]any{"role": "assistant", "tool_calls": []any{call("c1")},
				"content": []any{map[string]any{"type": "text", "text": "x"}}},
			map[string]any{"role": "tool", "tool_call_id": "c0", "content": "r0"},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r1"},
		}
		got, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Fatal("结构化 parts 不应合并")
		}
		if len(got) != 4 {
			t.Fatalf("应为 4 条，实得 %d", len(got))
		}
	})
}

var _ = reflect.DeepEqual // 保留 import（表格用例若增删不致编译错）
