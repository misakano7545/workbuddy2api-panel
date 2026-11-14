package server

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// continueReader：上游把多个 chunk 粘在一个 data 行里（issue #197）时拆成独立帧，正文照常
// 记账；粘在第一个位置的 error 帧经 StreamHint 仍触发首帧否决（不提交 200、可换号），
// 不会被当成「无 finish 断流」去续写。
func TestContinueReaderSplitsGluedFrames(t *testing.T) {
	frame := func(txt string) string {
		return `{"id":"a","choices":[{"index":0,"delta":{"content":"` + txt + `"}}]}`
	}
	open := func(stream string) *continueReader {
		up, _ := fakeContinueUpstream(t, func(int) (int, string) { return 200, "" })
		return newTestContinueReader(t, up, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			io.NopCloser(strings.NewReader(stream)))
	}

	cont := open("data: " + frame("x") + " " + frame("y") + "\n\ndata: [DONE]\n\n")
	out, _ := io.ReadAll(cont)
	if cont.text.String() != "xy" || strings.Count(string(out), "data: {") != 2 {
		t.Fatalf("粘帧应拆成两帧且正文都记账: text=%q\n%s", cont.text.String(), out)
	}

	errFrame := `{"error":{"message":"busy","code":"6021"}}`
	cont = open("data: " + errFrame + " " + frame("x") + "\n\ndata: [DONE]\n\n")
	rec := httptest.NewRecorder()
	err := upstream.StreamHint(rec, cont, nil)
	var hv *upstream.HeadVetoError
	if !errors.As(err, &hv) || rec.Body.Len() != 0 {
		t.Fatalf("粘在首位的 error 帧应首帧否决: err=%v body=%q", err, rec.Body.String())
	}
}
