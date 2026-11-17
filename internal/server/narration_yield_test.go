// narration_yield_test.go 等用户拍板的措辞挡住救援；技术叙述里的同类措辞不挡。
package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// narrationRescueCount 首段正文为 text、finish=stop 的工具类请求一共发了几次预告句救援。
func narrationRescueCount(t *testing.T, text string) int {
	t.Helper()
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"y1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"y1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":80,"total_tokens":268618}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	if _, err := io.ReadAll(newNarrateReader(t, up, agentBody, seg)); err != nil {
		t.Fatalf("read: %v", err)
	}
	return len(*reqs)
}

// 等用户拍板的措辞不论在哪一句都不救（#209 的事故形态）：确认请求后面常跟着草稿本身，只看最后
// 一句（或末尾 200B）会把收件人 / 主题行当成末句、把它判成承诺句救掉，模型就真把邮件发出去了。
func TestNarrationYieldMarkersStillSkipped(t *testing.T) {
	for _, text := range []string{
		"邮件已经写好，请确认后我再发送。\n\n收件人：all@example.com\n主题：Q3 计划",
		"邮件已经写好，确认后我再发送。\n\n收件人：all@example.com\n主题：Q3 计划",
		"请确认以下操作后我再执行\n- 删除 A\n- 删除 B",
		"是否需要我现在把邮件发出去？\n\n收件人：all@example.com\n主题：Q3 计划",
		"要不要现在发送？\n\n收件人：all@example.com\n主题：Q3 计划",
		"是否需要我先发给你过目。草稿如下\n收件人：all@example.com",
		"三处改动都过了一遍。是否需要顺便把文档也更新",
		// 末句里带文件名 / 版本号的半角点：不能把「是否需要」切到前一句去。
		"三处都改完了。是否需要顺便改 config.go 里的超时",
		"要不要顺便把 v1.2 的配置也改掉",
	} {
		if n := narrationRescueCount(t, text); n != 0 {
			t.Errorf("正文在等用户拍板，不该续写（%q），实际 %d 次", text, n)
		}
	}
}

// 反方向：技术叙述里的「确认无误 / 确认后 / 请告知 / 可以吗 / 是否需要我们」不是在等用户，
// 尾部停在承诺句上照样要救（issue #245）。按整段认这些词会把长回复一条条挡掉。
func TestNarrationNarrativeYieldWordsStillRescued(t *testing.T) {
	long := strings.Repeat("我把三处改动逐行过了一遍，其中两点需要单独说明。", 12)
	for _, mid := range []string{
		"日志格式确认无误。",
		"staging 确认后再上线。",
		"如有问题请告知维护者。",
		"这个函数的行为可以吗？我查过调用方，结论是可以。",
		"是否需要我们更换钢丝绳要看趋势数据。",
	} {
		text := long + mid + "\n\n我将开始修改 internal/verify/check.go"
		if n := narrationRescueCount(t, text); n != 1 {
			t.Errorf("中段是技术叙述（%q）、尾部停在承诺句上，应救一次，实际 %d 次", mid, n)
		}
	}
}

// toolonly 会采纳工具调用：确认请求后面又跟了一句（"草稿放在 draft.md 里。"）时，只看最后一句
// 就会放行救援、把「发送」这一步替用户执行掉。这条救援按整段认等用户措辞。
func TestToolOnlySkipsWaitingForUserBeforeLastSentence(t *testing.T) {
	seg := `data: {"id":"t9","choices":[{"index":0,"delta":{"content":"邮件已写好，请确认后我再发送。草稿放在 draft.md 里。"}}]}` + "\n\n" +
		`data: {"id":"t9","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":40,"total_tokens":106547}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, segToolOnlyToolCall })
	if _, err := io.ReadAll(newToolOnlyReader(t, up, toolLoopBody, seg)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("模型在等用户确认，不该续写，实际 %d 次", len(*reqs))
	}
}
