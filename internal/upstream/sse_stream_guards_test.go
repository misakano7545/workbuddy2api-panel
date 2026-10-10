package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStripToolCallNamesNoIndexKeepsEveryName 无 index 字段的上游（mergeToolCallsChunk
// 专门为它写了分支，是同族真实形态）：同一帧里多个调用各自保留首片 name，不能挤进
// 同一个键互相删——第二个调用的 name 被删掉后，客户端拿到的是没有名字的调用、派发不了。
func TestStripToolCallNamesNoIndexKeepsEveryName(t *testing.T) {
	frame := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{
			map[string]any{"id": "call_a", "type": "function", "function": map[string]any{"name": "Bash", "arguments": `{"cmd":"ls"}`}},
			map[string]any{"id": "call_b", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"file":"x"}`}},
		},
	}}}}

	seen := map[string]bool{}
	stripToolCallNames(frame, seen)

	nameOf := func(f map[string]any, i int) string {
		tcs, _ := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)
		fn, _ := tcs[i].(map[string]any)["function"].(map[string]any)
		n, _ := fn["name"].(string)
		return n
	}
	if nameOf(frame, 0) != "Bash" || nameOf(frame, 1) != "Read" {
		t.Fatalf("无 index 帧的 name 被误删：%q / %q", nameOf(frame, 0), nameOf(frame, 1))
	}

	// 续帧（同 id、同样没有 index）仍按「每个调用只带一次 name」删键。
	next := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{
			map[string]any{"id": "call_b", "type": "function", "function": map[string]any{"name": "Read", "arguments": "y"}},
		},
	}}}}
	stripToolCallNames(next, seen)
	if got := nameOf(next, 0); got != "" {
		t.Fatalf("续帧的重复 name 应被删除，实际 %q", got)
	}
}

// TestStreamTailDoesNotAddSecondFinish 断流场景：正文里是原生标记（被还原成调用）+
// continue 层补的 length 截断终态。流尾兜底此前会再补一条 tool_calls，一条流两个
// finish_reason，后一个把「截断」洗成「正常收尾」。
func TestStreamTailDoesNotAddSecondFinish(t *testing.T) {
	content := mkBlock(mkInvoke("exec_command", mkParam("cmd", "ls -la")))

	finishes := func(t *testing.T, raw string) []string {
		t.Helper()
		repair := NewMarkupRepair(testTools, false)
		rec := httptest.NewRecorder()
		if err := StreamHint(rec, strings.NewReader(raw), nil, WithMarkupRepair(repair)); err != nil {
			t.Fatal(err)
		}
		if repair.Converted() == 0 {
			t.Fatalf("前置条件不成立：标记未被还原，测不到该路径（%s）", rec.Body.String())
		}
		var got []string
		for _, f := range viewSSE(t, rec.Body.String()) {
			if f.finish != "" {
				got = append(got, f.finish)
			}
		}
		return got
	}

	// 上游给了终态（这里是 continue 层合成的截断终态）→ 恰好一条，且必须还是 length。
	if got := finishes(t, upstreamSSEWithContent([]string{content}, "length")); len(got) != 1 || got[0] != "length" {
		t.Fatalf("一条流恰好一个终态且为 length，实际 %v", got)
	}
	// 上游从未给终态 → 兜底照旧要补 tool_calls（别把兜底一起关掉）。
	if got := finishes(t, upstreamSSEWithContent([]string{content}, "")); len(got) != 1 || got[0] != "tool_calls" {
		t.Fatalf("上游无终态时应兜底补 tool_calls，实际 %v", got)
	}
}
