package upstream

import (
	"encoding/json"
	"testing"
)

// TestReorderEarlyToolResults Hub issue #2 / PR #3 的形态：function_call_output
// 排在对应 function_call 之前（Codex 经 cc-switch 类本地代理转发 /v1/responses）。
// 出站前必须把结果挪到它的 assistant(tool_calls) 之后，否则上游 11148。
func TestReorderEarlyToolResults(t *testing.T) {
	call := func(id string) map[string]any {
		return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": id, "type": "function",
				"function": map[string]any{"name": "shell", "arguments": "{}"}},
		}}
	}
	tool := func(id, out string) map[string]any {
		return map[string]any{"role": "tool", "tool_call_id": id, "content": out}
	}
	user := func(s string) map[string]any { return map[string]any{"role": "user", "content": s} }
	roles := func(msgs []any) []string {
		var got []string
		for _, raw := range msgs {
			m, _ := raw.(map[string]any)
			r, _ := m["role"].(string)
			if r == "tool" {
				id, _ := m["tool_call_id"].(string)
				r += ":" + id
			} else if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				tc, _ := tcs[0].(map[string]any)
				id, _ := tc["id"].(string)
				r += ":" + id
			}
			got = append(got, r)
		}
		return got
	}
	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("长度不符 got=%v want=%v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("第 %d 条不符 got=%v want=%v", i, got, want)
			}
		}
	}

	cases := []struct {
		name   string
		in     []any
		want   []string
		change bool
	}{
		{
			name:   "单个早到结果",
			in:     []any{user("run"), tool("c0", "ok"), call("c0"), user("next")},
			want:   []string{"user", "assistant:c0", "tool:c0", "user"},
			change: true,
		},
		{
			name:   "顺序本就正确（零改动）",
			in:     []any{user("run"), call("c0"), tool("c0", "ok")},
			want:   []string{"user", "assistant:c0", "tool:c0"},
			change: false,
		},
		{
			name:   "同批两条早到，保持原相对顺序",
			in:     []any{tool("c0", "a"), tool("c1", "b"), call2("c0", "c1")},
			want:   []string{"assistant:c0", "tool:c0", "tool:c1"},
			change: true,
		},
		{
			name:   "正常结果先出、早到结果补在同批之后",
			in:     []any{user("run"), tool("c1", "early"), call2("c0", "c1"), tool("c0", "late"), user("next")},
			want:   []string{"user", "assistant:c0", "tool:c0", "tool:c1", "user"},
			change: true,
		},
		{
			name:   "孤儿结果不动（交给 cleanup 删）",
			in:     []any{user("run"), tool("zz", "orphan")},
			want:   []string{"user", "tool:zz"},
			change: false,
		},
		{
			name:   "声称者是最后一条消息：循环结束时仍要补上",
			in:     []any{user("run"), tool("c0", "ok"), call("c0")},
			want:   []string{"user", "assistant:c0", "tool:c0"},
			change: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := reorderEarlyToolResults(tc.in)
			if changed != tc.change {
				t.Fatalf("changed=%v want=%v", changed, tc.change)
			}
			eq(t, roles(out), tc.want)
		})
	}
}

func call2(ids ...string) map[string]any {
	var tcs []any
	for _, id := range ids {
		tcs = append(tcs, map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": "shell", "arguments": "{}"}})
	}
	return map[string]any{"role": "assistant", "content": "", "tool_calls": tcs}
}

// TestPrepareBodyReordersEarlyToolResults 端到端：走完整出站管线后，早到结果
// 已经落在它自己的 assistant 之后，且没有被 cleanupOrphanToolCalls 误删。
func TestPrepareBodyReordersEarlyToolResults(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[` +
		`{"role":"user","content":"run"},` +
		`{"role":"tool","tool_call_id":"c0","content":"ok"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c0","type":"function",` +
		`"function":{"name":"shell","arguments":"{}"}}]},` +
		`{"role":"user","content":"next"}]}`
	out := PrepareBodyOpt([]byte(body), false)
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出站 body 解析失败: %v\n%s", err, out)
	}
	var seq []string
	for _, m := range obj.Messages {
		role, _ := m["role"].(string)
		if role == "tool" {
			id, _ := m["tool_call_id"].(string)
			seq = append(seq, "tool:"+id)
			continue
		}
		if _, ok := m["tool_calls"]; ok {
			seq = append(seq, "assistant:"+role)
			continue
		}
		seq = append(seq, role)
	}
	// 首条可能是网关补的 system，只在末尾比对关键三连。
	joined := ""
	for _, s := range seq {
		joined += s + " "
	}
	if !containsSeq(seq, []string{"assistant:assistant", "tool:c0"}) {
		t.Fatalf("早到结果没有落到 assistant 之后，实际顺序: %s", joined)
	}
	if len(seq) < 4 {
		t.Fatalf("结果被误删（cleanup 不应删掉两侧 id 都在的配对）: %s", joined)
	}
}

func containsSeq(hay, needle []string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		ok := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
