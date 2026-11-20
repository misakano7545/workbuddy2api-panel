package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestSpeechAndTranscribeWireFormat 锁住两条语音请求的出站形状。
// 重点是 ASR 的 Content-Type：CommonHeaders 会给所有请求写死 application/json，
// 把 multipart 的 boundary 抹掉后上游只能回 15052「failed to parse multipart form」
// （2026-11-20 真号实测踩到过），所以这里必须按真实客户端身份构造再断言。
func TestSpeechAndTranscribeWireFormat(t *testing.T) {
	var asrCT string
	var asrAudio string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case SpeechPath:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"speech.audio.delta\",\"audio\":{\"data\":\"AAEC\"}}\n\n")
			io.WriteString(w, "data: {\"type\":\"speech.audio.done\"}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		case ASRPath:
			asrCT = r.Header.Get("Content-Type")
			mr, err := r.MultipartReader()
			if err != nil {
				t.Errorf("上游收到的不是 multipart: %v (Content-Type=%q)", err, asrCT)
				return
			}
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				if p.FormName() == "audio" {
					b, _ := io.ReadAll(p)
					asrAudio = string(b)
				}
			}
			io.WriteString(w, `{"code":0,"text":"识别结果"}`)
		default:
			t.Errorf("意料之外的路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	pcm, err := c.Speech(context.Background(), a, "你好", "")
	if err != nil {
		t.Fatalf("Speech: %v", err)
	}
	if string(pcm) != "\x00\x01\x02" {
		t.Fatalf("PCM = %q, want %%q 解出的 \\x00\\x01\\x02", pcm)
	}

	text, err := c.Transcribe(context.Background(), a, "a.wav", "audio/wav", []byte("RIFFdata"))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "识别结果" {
		t.Fatalf("text = %q", text)
	}
	if !strings.HasPrefix(asrCT, "multipart/form-data; boundary=") {
		t.Fatalf("ASR Content-Type = %q, 必须是带 boundary 的 multipart", asrCT)
	}
	if asrAudio != "RIFFdata" {
		t.Fatalf("audio 字段 = %q", asrAudio)
	}
}
