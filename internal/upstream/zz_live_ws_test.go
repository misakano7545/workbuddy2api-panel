package upstream

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestZZLiveProbeSpeechVoices 遍历音色取值，看上游对 voice 是校验、回退还是静默忽略；
// 同时在 global 号上试一次，确认海外域有没有这个端点。
//
//	WB_PROBE_AUTHDIR=/opt/.../auths WB_PROBE_UID=<cn-uid> WB_PROBE_GLOBAL_UID=<global-uid> ./x.test -test.run TestZZLiveProbeSpeechVoices -test.v
func TestZZLiveProbeSpeechVoices(t *testing.T) {
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

	voices := []string{
		"",                            // 空 → 网关回落官方默认
		TTSDefaultVoice,               // 默认音色显式传
		"f191_male_cn_tts2.5_flash",   // 猜测的男声同族
		"f191_female_en_tts2.5_flash", // 猜测的英文同族
		"nonexistent_voice_xyz",       // 明显非法
	}
	for _, v := range voices {
		pcm, err := c.Speech(ctx, acct, "音色测试，一、二、三。", v)
		name := v
		if name == "" {
			name = "(空=默认)"
		}
		if err != nil {
			t.Logf("voice %-30s ERR %v", name, err)
			continue
		}
		t.Logf("voice %-30s OK pcm=%d B (%.2fs)", name, len(pcm), float64(len(pcm))/2/TTSSampleRate)
	}

	// 海外域：端点是否存在、鉴权是否同一套。
	gdir := os.Getenv("WB_PROBE_GLOBAL_UID")
	if gdir == "" {
		return
	}
	g := loadProbeAuth(t, dir, gdir)
	t.Logf("global 账号 %s realm=%s base=%s", g.UID, g.Realm(), c.chatBase(g))
	if err := c.RefreshTokenIfNeeded(g, 10*time.Minute); err != nil {
		t.Logf("global 刷新 token 失败: %v", err)
		return
	}
	pcm, err := c.Speech(ctx, g, "海外域语音测试。", "")
	if err != nil {
		t.Logf("global TTS ERR %v", err)
	} else {
		t.Logf("global TTS OK pcm=%d B", len(pcm))
	}
}

// TestZZLiveProbeSpeechStream 真机验证 WebSocket 流式 ASR（/clientcap/v2/asr/stream）：
// 用 TTS 合成音频 → 重采样到 16kHz → 按 100ms 分片推 → 空帧收尾 → 读 final。
//
// 官方客户端的音频格式是从渲染进程反查的：s16le 单声道 16kHz、每片 100ms、空二进制帧结束。
func TestZZLiveProbeSpeechStream(t *testing.T) {
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

	const want = "流式语音识别测试，今天天气不错。"
	pcm24, err := c.Speech(ctx, acct, want, "")
	if err != nil {
		t.Fatalf("TTS 失败（流式 ASR 需要素材）: %v", err)
	}
	pcm16 := resample16k(pcm24)
	t.Logf("素材 pcm24=%d B → pcm16=%d B (%.2fs @16kHz)", len(pcm24), len(pcm16), float64(len(pcm16))/2/16000)

	wsURL := c.chatBase(acct) + "/clientcap/v2/asr/stream?session_id=" +
		fmt.Sprintf("probe-%d", time.Now().UnixNano()) + "&source=desktop&interim_results=1"
	wsURL = strings.Replace(strings.Replace(wsURL, "https://", "wss://", 1), "http://", "ws://", 1)

	ws, err := dialWS(wsURL, acct)
	if err != nil {
		t.Fatalf("WS 握手失败: %v", err)
	}
	defer ws.Close()
	t.Logf("WS 已连接 %s", wsURL)

	// 100ms 一片（16kHz * 2B * 0.1s = 3200 B）。
	const chunk = 3200
	for off, seq := 0, 1; off < len(pcm16); off, seq = off+chunk, seq+1 {
		end := off + chunk
		if end > len(pcm16) {
			end = len(pcm16)
		}
		if _, err := ws.Write(pcm16[off:end]); err != nil {
			t.Fatalf("推第 %d 片失败: %v", seq, err)
		}
		time.Sleep(20 * time.Millisecond) // 模拟实时采集的节奏，避免一次性灌满
	}
	if _, err := ws.Write([]byte{}); err != nil { // 0 长度帧 = 说完了
		t.Fatalf("发结束帧失败: %v", err)
	}

	type frame struct {
		Code    any    `json:"code"`
		Text    string `json:"text"`
		Stable  bool   `json:"stable"`
		Final   bool   `json:"final"`
		VoiceID string `json:"voice_id"`
		Noise   bool   `json:"noise_reject"`
	}
	_ = ws.SetDeadline(time.Now().Add(40 * time.Second))
	var last string
	for {
		var raw []byte
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			t.Fatalf("读下行失败（已收到 %q）: %v", last, err)
		}
		var f frame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Logf("下行非 JSON: %q", truncate(string(raw), 200))
			continue
		}
		t.Logf("下行 voice_id=%q stable=%v final=%v noise=%v text=%q", f.VoiceID, f.Stable, f.Final, f.Noise, f.Text)
		last = f.Text
		if f.Final {
			if strings.TrimSpace(f.Text) == "" {
				t.Fatalf("final 帧文本为空（素材未识别）")
			}
			t.Logf("流式 ASR 成功：%q（期望近似 %q）", f.Text, want)
			return
		}
	}
}

// dialWS 按官方客户端的方式建 WS：Bearer + X-User-Id + X-Domain 头。
func dialWS(rawURL string, a *auth.Auth) (*websocket.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := &net.Dialer{Timeout: 20 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", host, &tls.Config{ServerName: u.Hostname()})
	if err != nil {
		return nil, err
	}
	origin := "https://" + u.Hostname()
	cfg, err := websocket.NewConfig(rawURL, origin)
	if err != nil {
		conn.Close()
		return nil, err
	}
	cfg.Header.Set("Authorization", "Bearer "+a.AccessToken)
	cfg.Header.Set("X-User-Id", a.UID)
	if d := a.DomainValue(); d != "" {
		cfg.Header.Set("X-Domain", d)
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	ws.PayloadType = websocket.BinaryFrame
	return ws, nil
}

// resample16k 24kHz s16le 单声道 → 16kHz（3:2 线性插值）。
// 只为探针准备素材；上游是否真按 16k 解码由返回文本是否正确来判定。
func resample16k(in []byte) []byte {
	n := len(in) / 2
	out := make([]byte, 0, n*2*2/3)
	sample := func(i int) float64 {
		if i < 0 || i >= n {
			return 0
		}
		return float64(int16(uint16(in[2*i]) | uint16(in[2*i+1])<<8))
	}
	for i := 0; ; i++ {
		pos := float64(i) * 1.5
		j := int(pos)
		if j+1 >= n {
			break
		}
		v := sample(j) + (sample(j+1)-sample(j))*(pos-float64(j))
		s := int16(v)
		out = append(out, byte(uint16(s)), byte(uint16(s)>>8))
	}
	return out
}
