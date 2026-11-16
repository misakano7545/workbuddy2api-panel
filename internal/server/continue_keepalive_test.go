// continue_keepalive_test.go 换段等待期不许静默（「wb 会中断、Trae 不中断」的协议差异）。
//
// 现象：同一客户端走 Trae 网关不中断，走本网关会中断。差别在换段：续写/救援段是一次
// 全新的上游 POST（十万级 prompt 的 prefill 常 3~15s），旧实现是**同步**发起的，
// 这段时间 Read() 被卡住，客户端一个字节都收不到；流式客户端有 idle 超时，静默到阈值
// 就主动断开——表现就是「答案说到一半突然中断」。
//
// 修法：换段请求异步发起，等待期内按 keepaliveEvery 补 ": keepalive" 注释帧。
// 本文件钉住三件事：等待期真的有保活、客户端可见间隔不超过阈值、终态仍然只有一个。
package server

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// slowSegUpstream 第 1 次立即返回首段，第 2 次先睡 delay 再返回续写段。
// script 在 Transport 的 roundTrip 里执行，这里 sleep 就等于「上游 prefill 很慢」。
func slowSegUpstream(t *testing.T, delay time.Duration, second string, secondStatus int) (*upstream.Client, *[]string) {
	t.Helper()
	return fakeContinueUpstream(t, func(call int) (int, string) {
		if call == 1 {
			return 200, testSeg1Text
		}
		time.Sleep(delay)
		return secondStatus, second
	})
}

// testContinueBody 首段就带 max_tokens：模拟客户端额度上限触发截断续写。
// 注意不要带 max_tokens：带额度 = 客户端只要这么长，canContinue 会按预期拒绝续写。
const testContinueBody = `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`

// readTimeline 读完整个流，返回每次 Read 拿到的时间戳与内容。
func readTimeline(r io.Reader) ([]time.Time, []string) {
	var stamps []time.Time
	var chunks []string
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			stamps = append(stamps, time.Now())
			chunks = append(chunks, string(buf[:n]))
		}
		if err != nil {
			return stamps, chunks
		}
	}
}

// TestContinueKeepaliveWhileOpeningSegment 换段等待 1.5s：期间必须持续有保活帧，
// 且客户端可见的最大间隔不超过 keepaliveEvery 的量级。
func TestContinueKeepaliveWhileOpeningSegment(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 200 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })

	up, _ := slowSegUpstream(t, 1500*time.Millisecond, testSeg2Text, 200)
	r := newTestContinueReader(t, up, testContinueBody, io.NopCloser(strings.NewReader(testSeg1Text)))

	start := time.Now()
	stamps, chunks := readTimeline(r)
	elapsed := time.Since(start)

	all := strings.Join(chunks, "")
	if !strings.Contains(all, "继续") {
		t.Fatalf("续写段正文没接上: %q", all)
	}
	if strings.Count(all, `"finish_reason":"stop"`) != 1 {
		t.Fatalf("终态必须恰好一个, got %d: %q", strings.Count(all, `"finish_reason":"stop"`), all)
	}
	if strings.Contains(all, `"finish_reason":"length"`) {
		t.Fatalf("首段的 length 不该外泄: %q", all)
	}

	// 等待期保活：1.5s 的窗口里按 200ms 一条，至少 4 条。
	ka := strings.Count(all, ": keepalive")
	if ka < 4 {
		t.Fatalf("换段等待期保活帧=%d want >=4（否则客户端 1.5s 零字节，会被 idle 超时掐断）: %q", ka, all)
	}

	// 客户端可见间隔：最长的两帧间隔不该接近整个等待期。
	maxGap := time.Duration(0)
	prev := start
	for _, ts := range stamps {
		if d := ts.Sub(prev); d > maxGap {
			maxGap = d
		}
		prev = ts
	}
	if maxGap > 600*time.Millisecond {
		t.Fatalf("客户端最长静默 %v 超过阈值（保活没生效）", maxGap)
	}
	if elapsed < 1400*time.Millisecond {
		t.Fatalf("测试没真的等待换段（elapsed=%v），断言无效", elapsed)
	}
}

// TestContinueSegmentFailureStillSingleTerminal 换段失败（500）时：
// 按首段终态收尾、保活照旧、不多出终态、不挂住。
func TestContinueSegmentFailureStillSingleTerminal(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 100 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })

	up, _ := slowSegUpstream(t, 400*time.Millisecond, "", 500)
	r := newTestContinueReader(t, up, testContinueBody, io.NopCloser(strings.NewReader(testSeg1Text)))

	stamps, chunks := readTimeline(r)
	all := strings.Join(chunks, "")
	if strings.Count(all, `"finish_reason"`) != 1 {
		t.Fatalf("换段失败后应恰好一个终态: %q", all)
	}
	if !strings.Contains(all, ": keepalive") {
		t.Fatalf("失败等待期也该有保活: %q", all)
	}
	if len(stamps) == 0 {
		t.Fatal("没有任何输出")
	}
}
