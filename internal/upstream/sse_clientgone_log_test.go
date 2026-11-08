package upstream

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureLog 捕获一段代码执行期间的 log 输出。
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// cutStream 上游说到一半就 EOF：有正文、无 finish_reason、无 [DONE]。
const cutStream = `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"半句"}}]}` + "\n\n"

// TestStreamCutWarnsWhenClientStillWaiting 客户端还在等（ctx 未取消）时，上游断流
// 必须落 WARN：这是真故障，运维要靠它发现。
func TestStreamCutWarnsWhenClientStillWaiting(t *testing.T) {
	rec := httptest.NewRecorder()
	out := captureLog(t, func() {
		if err := StreamHint(rec, strings.NewReader(cutStream), nil,
			WithClientGoneFn(func() bool { return false })); err != nil {
			t.Fatalf("StreamHint: %v", err)
		}
	})
	if !strings.Contains(out, "上游未给终态就结束") {
		t.Fatalf("客户端还在等时应记 WARN，实际日志=%q", out)
	}
	// 截断终态照常补：客户端至少知道被截断了。
	if !strings.Contains(rec.Body.String(), `"finish_reason":"`+TruncationFinishReason+`"`) {
		t.Fatalf("断流仍应补截断终态:\n%s", rec.Body.String())
	}
}

// TestStreamCutSilentWhenClientGone 客户端已经断开（pkill / 关页面 / 客户端 idle
// 超时掐线）时，上游只是被动跟随断连——这条不该记 WARN，否则「错误汇总」里会混进
// 与上游无关的噪音（线上实测 pkill 一次就刷一条，还落进「其他（未归类）」）。
func TestStreamCutSilentWhenClientGone(t *testing.T) {
	rec := httptest.NewRecorder()
	out := captureLog(t, func() {
		if err := StreamHint(rec, strings.NewReader(cutStream), nil,
			WithClientGoneFn(func() bool { return true })); err != nil {
			t.Fatalf("StreamHint: %v", err)
		}
	})
	if strings.Contains(out, "上游未给终态就结束") {
		t.Fatalf("客户端已断开时不该记 WARN，实际日志=%q", out)
	}
}

// TestStreamCutWarnsWithoutClientGoneFn 没有注入判定（老调用方）时保持原行为：
// 宁可多记一条，也不要漏掉真故障。
func TestStreamCutWarnsWithoutClientGoneFn(t *testing.T) {
	rec := httptest.NewRecorder()
	out := captureLog(t, func() {
		if err := StreamHint(rec, strings.NewReader(cutStream), nil); err != nil {
			t.Fatalf("StreamHint: %v", err)
		}
	})
	if !strings.Contains(out, "上游未给终态就结束") {
		t.Fatalf("未注入判定时应保持原行为（记 WARN），实际日志=%q", out)
	}
}
