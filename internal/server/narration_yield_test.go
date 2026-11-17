// narration_yield_test.go 等用户拍板的措辞在任何位置都挡住救援。
package server

import (
	"encoding/json"
	"io"
	"testing"
)

// 等用户拍板的措辞不论在哪一句都不救（#209 的事故形态）：确认请求后面常跟着草稿本身，只看最后
// 一句（或末尾 200B）会把收件人 / 主题行当成末句、把它判成承诺句救掉，模型就真把邮件发出去了。
// 「是否需要 / 要不要」用在问句或对我、你说话时同样不救；用在技术叙述里则不挡（见上一条）。
func TestNarrationYieldMarkersStillSkipped(t *testing.T) {
	for _, text := range []string{
		"邮件已经写好，请确认后我再发送。\n\n收件人：all@example.com\n主题：Q3 计划",
		"请确认以下操作后我再执行\n- 删除 A\n- 删除 B",
		"是否需要我现在把邮件发出去？\n\n收件人：all@example.com\n主题：Q3 计划",
		"是否需要我先发给你过目。草稿如下\n收件人：all@example.com",
		"三处改动都过了一遍。是否需要顺便把文档也更新",
		// 末句里带文件名 / 版本号的半角点：不能把「是否需要」切到前一句去。
		"三处都改完了。是否需要顺便改 config.go 里的超时",
		"要不要顺便把 v1.2 的配置也改掉",
	} {
		raw, _ := json.Marshal(text)
		seg := `data: {"id":"y1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
			`data: {"id":"y1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":80,"total_tokens":268618}}` + "\n\n" +
			"data: [DONE]\n\n"
		up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
		if _, err := io.ReadAll(newNarrateReader(t, up, agentBody, seg)); err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(*reqs) != 0 {
			t.Errorf("正文在等用户拍板，不该续写（%q），实际 %d 次", text, len(*reqs))
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
