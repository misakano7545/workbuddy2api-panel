package upstream

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"
)

// TestZZLiveProbeSpeech 真机验证官方语音端点（issue #277）：先用 TTS 合成一段音频，
// 再把这段音频喂回 ASR，形成自证闭环——不需要外部音频素材。
//
// 协议是官方包静态反查来的、没抓过包，所以必须在真号上跑一次才能确认
// base 路径、SSE 事件名、multipart 字段名、回程 JSON 形状。
//
// 只在设了 WB_PROBE_AUTHDIR 时有意义；普通 go test 自动跳过。
//
//	GOOS=linux GOARCH=amd64 go test -c -o x.test ./internal/upstream
//	WB_PROBE_AUTHDIR=/opt/.../auths WB_PROBE_UID=<uid> WB_PROBE_OUT=/tmp ./x.test -test.run TestZZLiveProbeSpeech -test.v
func TestZZLiveProbeSpeech(t *testing.T) {
	dir := os.Getenv("WB_PROBE_AUTHDIR")
	if dir == "" {
		t.Skip("未设 WB_PROBE_AUTHDIR，跳过真机探针")
	}
	acct := loadProbeAuth(t, dir, os.Getenv("WB_PROBE_UID"))
	t.Logf("账号 %s realm=%s", acct.UID, acct.Realm())

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err := c.RefreshTokenIfNeeded(acct, 10*time.Minute); err != nil {
		t.Fatalf("刷新 token 失败: %v", err)
	}

	pcm, err := c.Speech(ctx, acct, "你好，这是一段网关语音合成测试。", "")
	if err != nil {
		t.Fatalf("TTS 失败: %v", err)
	}
	t.Logf("TTS OK: pcm=%d B (约 %.2f 秒 @24kHz s16le)", len(pcm), float64(len(pcm))/2/TTSSampleRate)
	if len(pcm) < 2*TTSSampleRate/4 {
		t.Logf("警告: 音频不足 0.25 秒，可能是空音频")
	}

	wav := pcmToWAV(pcm)
	if out := os.Getenv("WB_PROBE_OUT"); out != "" {
		p := out + "/tts-probe.wav"
		if err := os.WriteFile(p, wav, 0o600); err != nil {
			t.Logf("写 %s 失败: %v", p, err)
		} else {
			t.Logf("已写出 %s (%d B)", p, len(wav))
		}
	}

	text, err := c.Transcribe(ctx, acct, "tts-probe.wav", "audio/wav", wav)
	if err != nil {
		t.Fatalf("ASR 失败: %v", err)
	}
	t.Logf("ASR OK: %q", text)
}

// pcmToWAV 给裸 PCM 套 44 字节 RIFF 头（24kHz 单声道 s16le），与 server 层同口径。
func pcmToWAV(pcm []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&b, binary.LittleEndian, uint32(TTSSampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(TTSSampleRate*2)) // byte rate
	binary.Write(&b, binary.LittleEndian, uint16(2))               // block align
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
