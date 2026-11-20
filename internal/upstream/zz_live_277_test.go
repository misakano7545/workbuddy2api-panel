package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestZZLiveProbe277 核 issue #277 里对方报的四条：ASR 3MB 上限、裸 PCM 的 15053、
// TTS 的 Accept 覆盖、SSE 错误事件形状。
func TestZZLiveProbe277(t *testing.T) {
	dir := os.Getenv("WB_PROBE_AUTHDIR")
	if dir == "" {
		t.Skip("未设 WB_PROBE_AUTHDIR，跳过真机探针")
	}
	acct := loadProbeAuth(t, dir, os.Getenv("WB_PROBE_UID"))
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	if err := c.RefreshTokenIfNeeded(acct, 10*time.Minute); err != nil {
		t.Fatalf("刷新 token 失败: %v", err)
	}
	pcm, err := c.Speech(ctx, acct, "你好，这是一次语音接口测试。", "")
	if err != nil {
		t.Fatalf("TTS 失败: %v", err)
	}
	t.Logf("素材 pcm=%d B", len(pcm))

	t.Run("ASR_裸PCM", func(t *testing.T) {
		_, err := c.Transcribe(ctx, acct, "raw.pcm", "audio/pcm", pcm)
		t.Logf("裸 PCM → %v", err)
	})

	t.Run("ASR_WAV", func(t *testing.T) {
		txt, err := c.Transcribe(ctx, acct, "a.wav", "audio/wav", pcmToWAV(pcm))
		t.Logf("WAV → %q err=%v", txt, err)
	})

	// 3MB 边界：上游文案说 under 3MB，测 2.9MB / 3.1MB / 4MB 各一次。
	big := pcmToWAV(pcm)
	pad := make([]byte, 0)
	for _, want := range []int{2900 << 10, 3100 << 10, 4000 << 10} {
		if want > len(big) {
			pad = make([]byte, want-len(big))
			_, _ = rand.Read(pad[:min(len(pad), 64<<10)])
		}
		body := append(append([]byte{}, big...), pad[:max(0, want-len(big))]...)
		t.Run(fmt.Sprintf("ASR_%dKB", want>>10), func(t *testing.T) {
			txt, err := c.Transcribe(ctx, acct, "big.wav", "audio/wav", body)
			t.Logf("%d B → %q err=%v", len(body), truncate(txt, 60), err)
		})
	}

	// TTS 的 Accept 被 ChatHeaders 覆盖成 application/json：上游到底看不看？
	for _, acc := range []string{"text/event-stream", "application/json"} {
		t.Run("TTS_Accept_"+strings.ReplaceAll(acc, "/", "_"), func(t *testing.T) {
			st, ct, head := rawSpeech(t, ctx, c, acct, acc, "Accept 测试。")
			t.Logf("Accept=%s → %d ct=%q 首行=%q", acc, st, ct, head)
		})
	}

	// 超长输入：看错误是走 HTTP 状态码还是 SSE 里的 error 事件。
	for _, n := range []int{500, 5000} {
		t.Run(fmt.Sprintf("TTS_长输入_%d字", n), func(t *testing.T) {
			st, ct, head := rawSpeech(t, ctx, c, acct, "text/event-stream", strings.Repeat("字", n))
			t.Logf("%d 字 → %d ct=%q 首行=%q", n, st, ct, head)
		})
	}
}

// rawSpeech 直接打 /v2/audio/speech，返回状态码、Content-Type 与首行正文（用于看错误形状）。
func rawSpeech(t *testing.T, ctx context.Context, c *Client, a *auth.Auth, accept, text string) (int, string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": TTSModel, "stream": true,
		"speech": map[string]any{"input": text, "voice": TTSDefaultVoice,
			"response_format": "pcm", "sample_rate": TTSSampleRate},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+SpeechPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("建请求失败: %v", err)
	}
	c.ChatHeaders(req, a, "", ChatMeta{})
	req.Header.Set("Accept", accept) // 故意放在 ChatHeaders 之后，测上游是否真的看 Accept
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req, a, false)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, resp.Header.Get("Content-Type"), truncate(strings.ReplaceAll(string(raw), "\n", "\\n"), 300)
}
