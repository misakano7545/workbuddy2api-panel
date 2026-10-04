package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsIncludesImageModels 面板「模型与档位」必须列出出图模型：
// 它们被目录按 tag（text-to-image）从对话列表剔掉，若不单列，面板上就看不到
// hunyuan-image-alpha / gpt-image-2.5-sunburst —— 用户不知道有哪些出图模型可调。
func TestPanelModelsIncludesImageModels(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			body := `{"code":0,"data":{"models":[` +
				`{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
				`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]},` +
				`{"id":"seedance-2.5","name":"Seedance-2.5","tags":["text-to-video","image-to-video"]}],` +
				`"agents":[{"name":"cli","models":["glm-5.2"]}]}}`
			if strings.HasSuffix(r.URL.Path, "/v3/config") {
				body = `{"code":0,"data":{"models":[` +
					`{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
					`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]},` +
					`{"id":"seedance-2.5","name":"Seedance-2.5","tags":["text-to-video"]}]}}`
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetCredits("c1", 1000, 0)

	// 目录解析会顺手把出图模型留存在 client 上（面板 API 直接读它，不再打上游）。
	if _, err := up.FetchModels(p.AuthByUID("c1")); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	pn.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var chat, image, video bool
	for _, m := range resp.Models {
		switch m["id"] {
		case "cn:glm-5.2":
			chat = true
			if m["image_generation"] == true {
				t.Errorf("对话模型不应带 image_generation：%v", m)
			}
		case "cn:seedance-2.5":
			video = m["video_generation"] == true
		case "cn:hunyuan-image-alpha":
			image = m["image_generation"] == true
			if m["name"] != "Hunyuan Image Alpha" {
				t.Errorf("出图模型应带名字：%v", m)
			}
		}
	}
	if !chat || !image || !video {
		t.Fatalf("面板模型列表缺项：chat=%v image=%v video=%v（%d 条）", chat, image, video, len(resp.Models))
	}
}

// fakeRoundTripper 测试用 http.RoundTripper（本包内没有现成的，server 包那个不跨包）。
type fakeRoundTripper func(*http.Request) (*http.Response, error)

func (f fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
