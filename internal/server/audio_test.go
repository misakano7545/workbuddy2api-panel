package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// audioFake 记录落点与出站原文，回一段固定响应。
type audioFake struct {
	paths []string
	body  []byte // 最近一次出站请求体
	ctype string // 响应 Content-Type
	resp  []byte // 响应体
}

func (f *audioFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			f.paths = append(f.paths, r.URL.Path)
			f.body = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{f.ctype}},
				Body:       io.NopCloser(bytes.NewReader(f.resp)),
			}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

// ttsFrame 拼一条 SSE data 行。
func ttsFrame(v any) string {
	raw, _ := json.Marshal(v)
	return "data: " + string(raw) + "\n\n"
}

// TestSpeechWrapsPCMIntoWAV POST /v1/audio/speech：上游 SSE 的 base64 PCM 分片拼成
// 裸 PCM，再套 44 字节 RIFF 头返回 audio/wav（官方只给 PCM，不给封装）。
func TestSpeechWrapsPCMIntoWAV(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	sse := ttsFrame(map[string]any{"type": "speech.audio.delta",
		"audio": map[string]string{"data": base64.StdEncoding.EncodeToString(pcm[:4])}}) +
		ttsFrame(map[string]any{"type": "speech.audio.delta",
			"audio": map[string]string{"data": base64.StdEncoding.EncodeToString(pcm[4:])}}) +
		ttsFrame(map[string]any{"type": "speech.audio.done"}) +
		"data: [DONE]\n\n"
	f := &audioFake{ctype: "text/event-stream", resp: []byte(sse)}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client(), TTSEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"你好","response_format":"wav"}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("Content-Type=%q want audio/wav", got)
	}
	out := rec.Body.Bytes()
	if len(out) != 44+len(pcm) {
		t.Fatalf("WAV 长度=%d want %d（44 字节头 + %d 字节 PCM）", len(out), 44+len(pcm), len(pcm))
	}
	if string(out[:4]) != "RIFF" || string(out[8:12]) != "WAVE" || string(out[36:40]) != "data" {
		t.Fatalf("WAV 头不对: %q", out[:44])
	}
	if !bytes.Equal(out[44:], pcm) {
		t.Fatalf("PCM 分片拼接不对: %v", out[44:])
	}
	if f.paths[0] != "/v2/audio/speech" {
		t.Fatalf("落点应为 /v2/audio/speech: %v", f.paths)
	}
	var sent struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Speech struct {
			Input          string `json:"input"`
			Voice          string `json:"voice"`
			ResponseFormat string `json:"response_format"`
			SampleRate     int    `json:"sample_rate"`
		} `json:"speech"`
	}
	if err := json.Unmarshal(f.body, &sent); err != nil {
		t.Fatalf("出站请求体不是 JSON: %v (%s)", err, f.body)
	}
	// 模型/音色/格式/采样率必须逐字是官方的，否则上游直接拒。
	if sent.Model != upstream.TTSModel || !sent.Stream || sent.Speech.Input != "你好" ||
		sent.Speech.Voice != upstream.TTSDefaultVoice || sent.Speech.ResponseFormat != "pcm" ||
		sent.Speech.SampleRate != upstream.TTSSampleRate {
		t.Fatalf("出站请求体不符官方形态: %+v", sent)
	}
}

// TestSpeechDisabledReturns403 默认关闭：回 403 tts_disabled（功能存在、只是没开）。
func TestSpeechDisabledReturns403(t *testing.T) {
	f := &audioFake{ctype: "text/event-stream"}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"input":"hi"}`)))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "tts_disabled") {
		t.Fatalf("code=%d body=%s want 403 tts_disabled", rec.Code, rec.Body.String())
	}
	if len(f.paths) != 0 {
		t.Fatalf("关闭时不得打上游: %v", f.paths)
	}
}

// TestTranscriptionsForwardsAudio POST /v1/audio/transcriptions：multipart 的 file 字段
// 原样转发成上游的 audio 字段，回 {"text": ...}。
func TestTranscriptionsForwardsAudio(t *testing.T) {
	f := &audioFake{ctype: "application/json", resp: []byte(`{"code":0,"text":"今天天气不错"}`)}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client(), ASREnabled: true})

	wav := []byte("RIFF....WAVEfmt ")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(wav); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if got["text"] != "今天天气不错" {
		t.Fatalf("text 不符: %v", got["text"])
	}
	if f.paths[0] != "/agenttool/v1/asr" {
		t.Fatalf("落点应为 /agenttool/v1/asr: %v", f.paths)
	}
	// 上游要的字段名是 audio（不是 OpenAI 的 file）：转过去时改了名，字节必须原样。
	if !strings.Contains(string(f.body), `name="audio"`) || !bytes.Contains(f.body, wav) {
		t.Fatalf("上游收到的 multipart 不含 audio 字段或音频被改动: %s", f.body)
	}
}

// TestTranscriptionsRejectsOversizeAudio 上游硬上限 3MB（实测 3.1MB 回 15052）；
// 入口直接 413，别让上游把尺寸问题报成「multipart 解析失败」。
func TestTranscriptionsRejectsOversizeAudio(t *testing.T) {
	f := &audioFake{ctype: "application/json"}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client(), ASREnabled: true})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(make([]byte, 3<<20+1)); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "audio_too_large") {
		t.Fatalf("code=%d body=%s want 413 audio_too_large", rec.Code, rec.Body.String())
	}
	if len(f.paths) != 0 {
		t.Fatalf("超限时不得打上游: %v", f.paths)
	}
}

// TestTranscriptionsCapsRawBody 超大 body 必须在落盘前被钳住（不是解析完再按文件大小拒）：
// 断言 413、不打上游，且实际读到的字节不超过 maxAudioBytes+audioBodySlack。
func TestTranscriptionsCapsRawBody(t *testing.T) {
	f := &audioFake{ctype: "application/json"}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client(), ASREnabled: true})

	const total = 64 << 20
	const head = "--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n" +
		"Content-Type: audio/wav\r\n\r\n"
	const tail = "\r\n--B--\r\n"
	body := io.MultiReader(strings.NewReader(head), io.LimitReader(zeroReader{}, total), strings.NewReader(tail))
	cr := &countingReader{r: body}
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", cr)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "audio_too_large") {
		t.Fatalf("code=%d body=%s want 413 audio_too_large", rec.Code, rec.Body.String())
	}
	if len(f.paths) != 0 {
		t.Fatalf("超限时不得打上游: %v", f.paths)
	}
	// MaxBytesReader 要多读 1 字节才知道超限，故上限 +1。
	if limit := maxAudioBytes + audioBodySlack + 1; cr.n > limit {
		t.Fatalf("读了 %d 字节，超过上限 %d（说明先落盘后拒）", cr.n, limit)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestTranscriptionsDisabledReturns403 默认关闭时回 403 asr_disabled。
func TestTranscriptionsDisabledReturns403(t *testing.T) {
	f := &audioFake{ctype: "application/json"}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader("")))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "asr_disabled") {
		t.Fatalf("code=%d body=%s want 403 asr_disabled", rec.Code, rec.Body.String())
	}
}
