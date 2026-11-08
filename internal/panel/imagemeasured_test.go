package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
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
				`{"id":"hy-image-3.5","name":"HY-Image-3.5","tags":["text-to-image","image-to-image"]}]}}`
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

	// 账本里有两次出图实扣（0.27 + 0.27）→ 实测单价 0.27（hy-image-3.5 实测每张固定 0.27）。
	rec := usage.New("")
	rec.Add(time.Now(), "global", "g1", "hy-image-3.5", usage.Delta{
		Credit: 0.27, HasCredit: true, LatencyMs: 20000, HasLatency: true,
	}, true)
	rec.Add(time.Now(), "global", "g1", "hy-image-3.5", usage.Delta{
		Credit: 0.27, HasCredit: true, LatencyMs: 24000, HasLatency: true,
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
		if m["id"] == "global:hy-image-3.5" {
			img = m
		}
	}
	if img == nil {
		t.Fatalf("图片模型未列出：%s", rec2.Body.String())
	}
	if got := img["credits"]; got != "" {
		t.Errorf("上游没报倍率时 credits 必须保持空（牌价与实测分列），得到 %v", got)
	}
	if got := img["measured_credit"]; got != "0.27" {
		t.Errorf("measured_credit=%v want 0.27（账本 credits/credit_samples）", got)
	}
	if got := img["credit_source"]; got != "usage_receipt" {
		t.Errorf("credit_source=%v want usage_receipt（无牌价，来源只有回执）", got)
	}
}

// TestPanelImageModelCreditWithCatalogPrice 有牌价时也必须带实扣，且分档可见。
//
// 背景：2026-10-18 复测时 CN 目录牌价 x5.00、实扣每张 5.71，旧逻辑只在 credits 为空
// 时填 measured_credit，面板显示 5.00 把实付盖住。**2026-10-22 复测目录已不再给
// 出图模型报 credits**（条目只剩 id/name/tags），所以这条从「现场」退化为防御性回归：
// 一旦上游重新下发牌价，牌价与实扣必须并列出、不得互相覆盖。
func TestPanelImageModelCreditWithCatalogPrice(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			body := `{"code":0,"data":{"models":[` +
				`{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
				`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"],"credits":"x5.00"}]}}`
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:        "https://cn.example",
		ChatBaseGlobal:    "https://global.example",
		BillingBaseGlobal: "https://global.example",
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetCredits("c1", 1000, 0)

	if _, err := up.FetchModels(p.AuthByUID("c1")); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	// 两次不同尺寸的出图，实扣都是 5.71（CN 每张固定价）。
	up.RecordImageCharge("cn", "hunyuan-image-alpha", "1024x1024", "", 5.71, 0, 0)
	up.RecordImageCharge("cn", "hunyuan-image-alpha", "512x512", "", 5.71, 0, 0)

	rec := usage.New("")
	rec.Add(time.Now(), "cn", "c1", "hunyuan-image-alpha", usage.Delta{Credit: 5.71, HasCredit: true}, true)

	pn := New(Config{Version: "test", Pool: p, Upstream: up, Usage: rec})
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
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
		if m["id"] == "cn:hunyuan-image-alpha" {
			img = m
		}
	}
	if img == nil {
		t.Fatalf("图片模型未列出：%s", rec2.Body.String())
	}
	if got := img["credits"]; got != "x5.00" {
		t.Errorf("credits=%v want 牌价 x5.00（不得被实扣覆盖）", got)
	}
	if got := img["measured_credit"]; got != "5.71" {
		t.Errorf("measured_credit=%v want 5.71（有牌价时也必须带实扣）", got)
	}
	if got := img["credit_source"]; got != "catalog+usage_receipt" {
		t.Errorf("credit_source=%v want catalog+usage_receipt", got)
	}
	variants, ok := img["charge_variants"].([]any)
	if !ok || len(variants) != 2 {
		t.Fatalf("charge_variants=%v want 2 档（1024x1024 / 512x512）", img["charge_variants"])
	}
	last, ok := img["charge_last"].(map[string]any)
	if !ok {
		t.Fatalf("charge_last 缺失：%v", img["charge_last"])
	}
	if got := last["credit"]; got != 5.71 {
		t.Errorf("charge_last.credit=%v want 5.71", got)
	}
}
