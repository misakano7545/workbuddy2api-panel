package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// 图片模型上游不报倍率（v3 目录条目里连 credits 键都没有），面板那一格用我们账本里的
// 实扣值兜底：/panel/api/models 的该条目应带 measured_credit = credits/credit_samples。
// 牌价（credits）与实测单价必须分列——两者来源不同，混在一起就是把观测当上游报价。
func TestPanelImageModelMeasuredCreditFallback(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			// 桌面端目录：图片模型只有 id/name/tags（实测形状），无 credits 键。
			body := `{"code":0,"data":{"models":[` +
				`{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
				`{"id":"gpt-image-2.5-sunburst","name":"GPT-Image-2.5-Sunburst","tags":["text-to-image","image-to-image"]}]}}`
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseGlobal:    "https://global.example",
		BillingBaseGlobal: "https://global.example",
		GlobalEnabled:     true,
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 1000, 0)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	if _, err := up.FetchModels(p.AuthByUID("g1")); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	// 账本里有两次出图实扣（0.55 + 0.55）→ 实测单价 0.55。
	rec := usage.New("")
	rec.Add(time.Now(), "global", "g1", "gpt-image-2.5-sunburst", usage.Delta{
		Credit: 0.55, HasCredit: true, LatencyMs: 20000, HasLatency: true,
	}, true)
	rec.Add(time.Now(), "global", "g1", "gpt-image-2.5-sunburst", usage.Delta{
		Credit: 0.55, HasCredit: true, LatencyMs: 24000, HasLatency: true,
	}, true)

	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up, Usage: rec})
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	pn.ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var img map[string]any
	for _, m := range resp.Models {
		if m["id"] == "global:gpt-image-2.5-sunburst" {
			img = m
		}
	}
	if img == nil {
		t.Fatalf("图片模型未列出：%s", rec2.Body.String())
	}
	if got := img["credits"]; got != "" {
		t.Errorf("上游没报倍率时 credits 必须保持空（牌价与实测分列），得到 %v", got)
	}
	if got := img["measured_credit"]; got != "0.55" {
		t.Errorf("measured_credit=%v want 0.55（账本 credits/credit_samples）", got)
	}
}
