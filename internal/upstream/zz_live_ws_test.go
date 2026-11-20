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
	streamASR(t, c, acct, pcm16, want)

	// 同一个素材再打一遍海外域：确认 /clientcap 是否也存在（素材仍由 CN 号合成）。
	if g := os.Getenv("WB_PROBE_GLOBAL_UID"); g != "" {
		gacct := loadProbeAuth(t, dir, g)
		if err := c.RefreshTokenIfNeeded(gacct, 10*time.Minute); err == nil {
			t.Logf("--- global 账号 %s base=%s", gacct.UID, c.chatBase(gacct))
			streamASR(t, c, gacct, pcm16, want)
		}
	}
}

// streamASR 把 16kHz PCM 按官方的节奏推给 /clientcap/v2/asr/stream，读到最后一条 final。
func streamASR(t *testing.T, c *Client, acct *auth.Auth, pcm16 []byte, want string) {
	t.Helper()
	r := wsText(t, c, acct, pcm16, 3200, 20*time.Millisecond, 15*time.Second)
	if strings.TrimSpace(r.text) == "" {
		t.Fatalf("final 帧文本为空（素材未识别）")
	}
	t.Logf("流式 ASR 成功：%q（期望近似 %q，final=%d 帧）", r.text, want, len(r.finals))
}

type wsResult struct {
	text     string   // 所有 final 帧按到达顺序拼接
	finals   []string // 每条 final 的原文
	interims int
	stable   int // stable=true 的中间帧数
	noise    int // noise_reject 帧数
	voiceID  string
	elapsed  time.Duration
	closed   bool // 服务端主动断开；false 表示静默 idle 后我方放弃
}

// wsText 按 chunk 字节一片推 pcm16，收完所有 final 帧。
// gap 为每片之间的等待（0 = 一口气写完，用于压缓冲上限）；服务端静默 idle 视为流结束。
func wsText(t *testing.T, c *Client, acct *auth.Auth, pcm16 []byte, chunk int, gap, idle time.Duration) wsResult {
	t.Helper()
	var r wsResult
	start := time.Now()
	wsURL := c.chatBase(acct) + "/clientcap/v2/asr/stream?session_id=" +
		fmt.Sprintf("probe-%d", time.Now().UnixNano()) + "&source=desktop&interim_results=1"
	wsURL = strings.Replace(strings.Replace(wsURL, "https://", "wss://", 1), "http://", "ws://", 1)

	ws, err := dialWS(wsURL, acct)
	if err != nil {
		t.Fatalf("WS 握手失败: %v", err)
	}
	defer ws.Close()
	t.Logf("WS 已连接 %s", wsURL)

	if chunk <= 0 || chunk > len(pcm16) {
		chunk = len(pcm16)
	}
	pushStart := time.Now()
	for off := 0; off < len(pcm16); off += chunk {
		end := off + chunk
		if end > len(pcm16) {
			end = len(pcm16)
		}
		if _, err := ws.Write(pcm16[off:end]); err != nil {
			t.Logf("推第 %d 片（offset %d）失败: %v", off/chunk+1, off, err)
			break
		}
		if gap > 0 {
			time.Sleep(gap)
		}
	}
	if _, err := ws.Write([]byte{}); err != nil { // 0 长度帧 = 说完了
		t.Logf("发结束帧失败: %v", err)
	}
	t.Logf("推完 %d B（chunk=%d，用时 %s），等 final…", len(pcm16), chunk, time.Since(pushStart).Round(time.Millisecond))

	type frame struct {
		Code    any    `json:"code"`
		Msg     string `json:"msg"`
		Text    string `json:"text"`
		Stable  bool   `json:"stable"`
		Final   bool   `json:"final"`
		VoiceID string `json:"voice_id"`
		Noise   bool   `json:"noise_reject"`
	}
	for {
		_ = ws.SetDeadline(time.Now().Add(idle))
		var raw []byte
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			r.closed = true
			t.Logf("下行结束: %v", err)
			break
		}
		var f frame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Logf("下行非 JSON: %q", truncate(string(raw), 200))
			continue
		}
		if f.VoiceID != "" && r.voiceID == "" {
			r.voiceID = f.VoiceID
		}
		if f.Noise {
			r.noise++
		}
		if f.Final {
			r.finals = append(r.finals, f.Text)
			r.text += f.Text
			t.Logf("final 帧 #%d len=%d text=%q code=%v", len(r.finals), len([]rune(f.Text)), truncate(f.Text, 80), f.Code)
		} else {
			r.interims++
			if f.Stable {
				r.stable++
			}
			if r.interims <= 3 || r.interims%20 == 0 {
				t.Logf("中间帧 #%d stable=%v noise=%v text=%q", r.interims, f.Stable, f.Noise, truncate(f.Text, 60))
			}
		}
	}
	r.elapsed = time.Since(start)
	t.Logf("汇总：interim=%d(stable %d) final=%d noise=%d voice_id=%q 用时=%s 服务端主动断=%v 文本=%d 字",
		r.interims, r.stable, len(r.finals), r.noise, r.voiceID, r.elapsed.Round(time.Millisecond), r.closed, len([]rune(r.text)))
	return r
}

// TestZZLiveProbeStreamEdges 补三个上一轮没覆盖的边界：长语音、分片大小、噪声抑制帧。
func TestZZLiveProbeStreamEdges(t *testing.T) {
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

	speech := func(text string) []byte {
		pcm, err := c.Speech(ctx, acct, text, "")
		if err != nil {
			t.Fatalf("TTS 失败: %v", err)
		}
		out := resample16k(pcm)
		t.Logf("素材 %d 字 → pcm24=%d B → pcm16=%d B (%.2fs @16kHz)", len([]rune(text)), len(pcm), len(out), float64(len(out))/2/16000)
		return out
	}
	const shortWant = "流式语音识别测试，今天天气不错。"
	shortPCM := speech(shortWant)
	longText := strings.Repeat("这是长语音测试，用来观察流式识别在长时间推流下的表现。", 6)
	longPCM := speech(longText)

	// 分片大小：默认 100ms=3200B，另测 50ms / 300ms / 奇数 1001B / 整块。
	for _, tc := range []struct {
		name  string
		chunk int
		gap   time.Duration
	}{
		{"50ms-1600B", 1600, 10 * time.Millisecond},
		{"300ms-9600B", 9600, 60 * time.Millisecond},
		{"奇数-1001B", 1001, 5 * time.Millisecond},
		{"整块-一次写完", 0, 0},
	} {
		t.Run("分片_"+tc.name, func(t *testing.T) {
			r := wsText(t, c, acct, shortPCM, tc.chunk, tc.gap, 15*time.Second)
			if !strings.Contains(r.text, "语音识别") {
				t.Fatalf("chunk=%d 识别结果不含关键内容: %q", tc.chunk, r.text)
			}
			t.Logf("chunk=%d ✅ %q", tc.chunk, r.text)
		})
	}

	// 长语音：官方客户端是实时节奏（100ms 音频 / 100ms 间隔）。这里扫推流速度，
	// 看上游在“推得比说得快”时会不会丢段（素材是同一句重复 6 次，数命中次数即可）。
	for _, tc := range []struct {
		name string
		gap  time.Duration
	}{
		{"1x-实时", 100 * time.Millisecond},
		{"2x", 50 * time.Millisecond},
		{"5x", 20 * time.Millisecond},
	} {
		t.Run("长语音_"+tc.name, func(t *testing.T) {
			r := wsText(t, c, acct, longPCM, 3200, tc.gap, 25*time.Second)
			hit := strings.Count(r.text, "长语音测试")
			t.Logf("%s：识别 %d 字 / 期望 %d 字 / 「长语音测试」出现 %d 次（期望 %d）/ final=%d 帧 / 用时 %s",
				tc.name, len([]rune(r.text)), len([]rune(longText)), hit, strings.Count(longText, "长语音测试"),
				len(r.finals), r.elapsed.Round(time.Millisecond))
			if strings.TrimSpace(r.text) == "" {
				t.Errorf("%s：一条 final 都没拿到", tc.name)
			}
		})
	}
	t.Run("长语音_整块", func(t *testing.T) {
		r := wsText(t, c, acct, longPCM, 0, 0, 25*time.Second)
		t.Logf("整块推 %d B 结果：%q（final=%d noise=%d 断=%v）", len(longPCM), truncate(r.text, 300), len(r.finals), r.noise, r.closed)
	})

	// 整块推会静默丢数据，量一下从多少字节开始丢，给流式实现定每片上限。
	for _, n := range []int{524288, 524289, 655360, 786432} {
		if n > len(longPCM) {
			continue
		}
		t.Run(fmt.Sprintf("整块上限_%dB", n), func(t *testing.T) {
			r := wsText(t, c, acct, longPCM[:n], 0, 0, 15*time.Second)
			t.Logf("一次写 %d B（%.1fs 音频）→ 识别 %d 字，final=%d, 断=%v", n, float64(n)/32000, len([]rune(r.text)), len(r.finals), r.closed)
		})
	}

	// 极短素材与空流：客户端点一下就说完了，或压根没录到音。
	t.Run("极短_0.2s", func(t *testing.T) {
		r := wsText(t, c, acct, shortPCM[:6400], 3200, 20*time.Millisecond, 10*time.Second)
		t.Logf("0.2s 素材 → 正文=%q final=%d 帧 断=%v", truncate(r.text, 60), len(r.finals), r.closed)
	})
	t.Run("空流_只说完了", func(t *testing.T) {
		r := wsText(t, c, acct, nil, 3200, 0, 8*time.Second)
		t.Logf("空流 → 正文=%q final=%d 帧 noise=%d 断=%v", truncate(r.text, 60), len(r.finals), r.noise, r.closed)
	})

	// 噪声抑制：静音与低幅白噪声都不该产出正文。
	silence := make([]byte, 16000*2*3)
	noise := make([]byte, 16000*2*3)
	for i := 0; i+1 < len(noise); i += 2 {
		v := int16((i*37)%200 - 100)
		noise[i] = byte(v)
		noise[i+1] = byte(v >> 8)
	}
	for _, tc := range []struct {
		name string
		pcm  []byte
	}{{"静音-3s", silence}, {"低幅白噪声-3s", noise}} {
		t.Run("噪声_"+tc.name, func(t *testing.T) {
			r := wsText(t, c, acct, tc.pcm, 3200, 20*time.Millisecond, 8*time.Second)
			t.Logf("噪声 %s → noise_reject=%d 帧、正文=%q、断=%v", tc.name, r.noise, truncate(r.text, 120), r.closed)
			if r.noise == 0 && strings.TrimSpace(r.text) != "" {
				t.Logf("注意：上游既没打 noise_reject 也给了正文，噪声抑制未生效")
			}
		})
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
