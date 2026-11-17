// continue_headveto_test.go 首帧否决窗口里不补换段保活。
package server

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 首段一帧都没交给客户端（唯一的数据帧就是 finish=length 终态，被扣在 tail 里）时触发的空截断
// 救援，等待期补的保活注释会被 StreamHint 当作首行写出、提交 200——之后救援段首帧报错也换不了号，
// 只能以「200 + error 帧」收尾。客户端还没收到任何数据帧之前不该补保活：首帧否决必须照旧生效。
func TestContinuePendingKeepsHeadVetoWindow(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 5 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })

	seg0 := `data: {"id":"s0","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":1024,"total_tokens":1034}}` + "\n\n" +
		"data: [DONE]\n\n"
	errSeg := `data: {"error":{"message":"quota exceeded","type":"upstream_error","code":"quota"}}` + "\n\n" + "data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) {
		time.Sleep(100 * time.Millisecond) // 救援段预填充
		return 200, errSeg
	})
	body := `{"model":"glm-5.2","max_tokens":1024,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(seg0)))
	rec := httptest.NewRecorder()
	err := upstream.StreamHint(rec, r, nil)
	if len(*reqs) != 1 {
		t.Fatalf("前提失败：应触发一次空截断救援，实际 %d 次", len(*reqs))
	}
	var hv *upstream.HeadVetoError
	if !errors.As(err, &hv) {
		t.Fatalf("救援段首帧是错误帧，应首帧否决换号，实际 err=%v body=%q", err, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("首帧否决前不该写出任何字节，实际 %q", rec.Body.String())
	}
}
