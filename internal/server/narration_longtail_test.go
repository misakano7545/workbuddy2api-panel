// narration_longtail_test.go 无标点长正文只救第一人称承诺句。
package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// 无标点的长正文只救「最后一句是第一人称承诺」的：写完的长总结最后一行常常没有句号，放开长度
// 闸门后它们会被续写一段（还被告知「承诺的动作没执行」），每次都是整段上下文重发的计费请求。
func TestNarrationLongFinishedTailNotRescued(t *testing.T) {
	long := strings.Repeat("这一轮我把三处改动逐条过了一遍，结论如下。", 30) + "\n\n"
	for _, tail := range []string{
		"## 改动清单\n- 修复续写闸门\n- 新增回归测试",
		"| 文件 | 改动 |\n|---|---|\n| continue.go | 续写闸门 |",
		"全部测试通过 ✅",
		"任务已全部完成",
		"改动见 internal/server/continue.go",
		"接下来你可以运行 go test ./... 验证",
	} {
		text := long + tail
		if !narrationTailSuspends(text) || len(text) <= narrationMaxBytes {
			t.Fatalf("前提失败：%q 应是超长的无标点收尾", tail)
		}
		raw, _ := json.Marshal(text)
		seg := `data: {"id":"f1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
			`data: {"id":"f1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":268538,"completion_tokens":500,"total_tokens":269038}}` + "\n\n" +
			"data: [DONE]\n\n"
		up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
		if _, err := io.ReadAll(newNarrateReader(t, up, agentBody, seg)); err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(*reqs) != 0 {
			t.Errorf("长总结以 %q 收尾不该续写，实际 %d 次", tail, len(*reqs))
		}
	}
}
