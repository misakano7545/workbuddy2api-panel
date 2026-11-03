package server

import (
	"io"
	"strings"
	"testing"
)

// 续写层自己切上游的行，同样要把注释行补成独立帧：上游的 ": heartbeat" 后面直接跟
// "data: {...}"（没有空行），本层又吞掉分隔空行、只给 data 帧自产 "\n\n"，不补的话
// 下游按 "\n\n" 切帧拿到 ": heartbeat\ndata: ..." 粘连块、没有 "data: " 前缀，整帧丢弃
// —— 被丢的恰是 GLM 工具调用的首帧（见 PR #158）。
func TestContinueReaderSeparatesCommentFromNextFrame(t *testing.T) {
	raw := ": heartbeat\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"get_weather","arguments":""}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	r := newTestContinueReader(t, nil, `{}`, io.NopCloser(strings.NewReader(raw)))
	out, _ := io.ReadAll(r)
	if strings.Contains(string(out), ": heartbeat\ndata: ") {
		t.Fatalf("注释行与 data 帧粘连（下游会整帧丢弃）: %q", out)
	}
	if !strings.Contains(string(out), `"name":"get_weather"`) {
		t.Fatalf("工具调用首帧丢失: %q", out)
	}
}
