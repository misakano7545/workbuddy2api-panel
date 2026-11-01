// models_auth_test.go GET /v1/models 的鉴权矩阵回归。
//
// 契约：同一个 API Key，无论走 Anthropic 官方 SDK 的 x-api-key，还是 OpenAI 系
// 客户端的 Authorization: Bearer，都必须能列出模型。
//
// 背景（实案）：Hermes 的 Anthropic 适配器启动时会拉 GET /v1/models 做模型发现，
// 走 x-api-key；而该端点此前只挂 withAuth（只认 Bearer），于是每次启动都报
// "could not reach this custom endpoint's model listing"，模型名只能「未经校验
// 接受」。同一把钥匙能开 /v1/messages 的门却开不了 /v1/models 的门，是路由挂载
// 的对称性缺失——withAnthropicAuth 只应用在了两条 messages 路由上。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/httpauth"
)

// modelsAuthHandler 构造只用于鉴权判定的最小 Handler。
//
// 必须给假上游：modelList 在缓存失效时会起后台 goroutine 打上游拉目录，
// 真去连 codebuddy.cn 既慢又 flaky，而上游为 nil 会直接空指针崩掉整个包。
func modelsAuthHandler(t *testing.T, apiKey string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":200000,"maxOutputTokens":32000}]}}`, false
	})
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1"}),
		Upstream: up,
		APIKey:   apiKey,
	})
}

func TestModelsAcceptsAnthropicXAPIKey(t *testing.T) {
	resetDynamicModelsCache()
	h := modelsAuthHandler(t, "secret")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key 携带方式应放行，got %d body=%s", rec.Code, rec.Body)
	}
}

func TestModelsAcceptsBearer(t *testing.T) {
	resetDynamicModelsCache()
	h := modelsAuthHandler(t, "secret")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer 携带方式应放行，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestModelsRejectsWrongKey 两种携带方式下的错误密钥都必须 401。
func TestModelsRejectsWrongKey(t *testing.T) {
	h := modelsAuthHandler(t, "secret")

	for _, tc := range []struct {
		name   string
		authz  string
		header map[string]string
	}{
		{name: "x-api-key wrong", header: map[string]string{"x-api-key": "nope", "anthropic-version": "2023-06-01"}},
		{name: "bearer wrong", authz: "Bearer nope"},
		{name: "no credentials"},
		{name: "wrong scheme", authz: "Basic secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("应 401，got %d body=%s", rec.Code, rec.Body)
			}
		})
	}
}

// TestModelsErrorShapePerProtocol 401 的错误体形状必须跟请求方协议走：
// Anthropic 侧要 type=error/authentication_error，OpenAI 侧要 error.code=invalid_api_key。
// 形状错了客户端会解析失败，把「密钥不对」误报成「网关坏了」。
func TestModelsErrorShapePerProtocol(t *testing.T) {
	h := modelsAuthHandler(t, "secret")

	t.Run("anthropic shape", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("x-api-key", "nope")
		req.Header.Set("anthropic-version", "2023-06-01")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应体不是 JSON: %v body=%s", err, rec.Body)
		}
		if body.Type != "error" || body.Error.Type != "authentication_error" {
			t.Fatalf("Anthropic 侧错误形状不对: %s", rec.Body)
		}
	})

	t.Run("openai shape", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer nope")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var body struct {
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应体不是 JSON: %v body=%s", err, rec.Body)
		}
		if body.Error.Code != "invalid_api_key" {
			t.Fatalf("OpenAI 侧错误形状不对: %s", rec.Body)
		}
	})
}

// ---------------------------------------------------------------------------
// 命名推理 key（多 key + 每 key 策略）的 HTTP 级契约
// ---------------------------------------------------------------------------

// keysAuthHandler 构造带命名 key 表的最小 Handler。假上游只回 glm-5.2，方便观察
// 每 key 模型白名单对 /v1/models 的过滤。
func keysAuthHandler(t *testing.T, apiKey string, pol httpauth.Policy) (*Handler, string) {
	t.Helper()
	store, err := httpauth.NewKeyStore(filepath.Join(t.TempDir(), "api_keys.json"))
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	_, secret, err := store.Create("test-key", pol, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":200000,"maxOutputTokens":32000}]}}`, false
	})
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1"}),
		Upstream: up,
		APIKey:   apiKey,
		Keys:     store,
	}), secret
}

func doAuth(h *Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNamedKeyAuthenticatesReasoningSurface 主口令为空、只有命名 key 时，命名 key
// 能且只能进推理面。
func TestNamedKeyAuthenticatesReasoningSurface(t *testing.T) {
	resetDynamicModelsCache()
	h, secret := keysAuthHandler(t, "", httpauth.Policy{})

	if rec := doAuth(h, http.MethodGet, "/v1/models", secret); rec.Code != http.StatusOK {
		t.Fatalf("命名 key 应能列模型，got %d body=%s", rec.Code, rec.Body)
	}
	if rec := doAuth(h, http.MethodGet, "/status", secret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("命名 key 不该进 /status，got %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, "/v1/capacity?model=glm-5.2", secret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("命名 key 不该进 /v1/capacity，got %d", rec.Code)
	}
}

// TestAnonymousRejectedWhenKeysConfigured 空 api_key + 非空 key 表：匿名一律 401。
// 「配了钥匙却全开」是最危险的静默失效，必须堵住。
func TestAnonymousRejectedWhenKeysConfigured(t *testing.T) {
	resetDynamicModelsCache()
	h, _ := keysAuthHandler(t, "", httpauth.Policy{})
	if rec := doAuth(h, http.MethodGet, "/v1/models", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名应 401，got %d body=%s", rec.Code, rec.Body)
	}
	if rec := doAuth(h, http.MethodGet, "/status", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名进 /status 也应 401，got %d", rec.Code)
	}
}

// TestNamedKeyModelAllowlistFiltersList 每 key 白名单同样管发现面。
func TestNamedKeyModelAllowlistFiltersList(t *testing.T) {
	resetDynamicModelsCache()
	h, secret := keysAuthHandler(t, "", httpauth.Policy{Models: []string{"other-model"}})
	rec := doAuth(h, http.MethodGet, "/v1/models", secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "glm-5.2") {
		t.Fatalf("白名单外的模型不该出现在列表: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("列表应为空: %s", rec.Body)
	}
}

// TestNamedKeyModelAllowlistRejectsChat 每 key 白名单外的模型点名调用 400（不打上游）。
func TestNamedKeyModelAllowlistRejectsChat(t *testing.T) {
	h, secret := keysAuthHandler(t, "", httpauth.Policy{Models: []string{"other-model"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("白名单外模型应 400，got %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("错误码应为 model_unavailable: %s", rec.Body)
	}
}

// TestNamedKeyExpiredRejected 到期逐请求判定：不需要重启。
func TestNamedKeyExpiredRejected(t *testing.T) {
	store, err := httpauth.NewKeyStore(filepath.Join(t.TempDir(), "api_keys.json"))
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	_, secret, err := store.Create("old", httpauth.Policy{}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), Upstream: newFakeUpstream(t,
		func(string) (int, string, bool) { return 200, `{"code":0,"data":{"models":[]}}`, false }), Keys: store})
	rec := doAuth(h, http.MethodGet, "/v1/models", secret)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("过期 key 应 401 expired，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestNamedKeyDisabledRejected 面板禁用立刻生效。
func TestNamedKeyDisabledRejected(t *testing.T) {
	store, err := httpauth.NewKeyStore(filepath.Join(t.TempDir(), "api_keys.json"))
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	v, secret, err := store.Create("off", httpauth.Policy{}, time.Time{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	disabled := true
	if _, err := store.Update(v.ID, httpauth.UpdateInput{Disabled: &disabled}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), Upstream: newFakeUpstream(t,
		func(string) (int, string, bool) { return 200, `{"code":0,"data":{"models":[]}}`, false }), Keys: store})
	rec := doAuth(h, http.MethodGet, "/v1/models", secret)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("禁用 key 应 401 disabled，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestNamedKeyRPMRejected 第 2 个请求撞 RPM=1 → 429。
func TestNamedKeyRPMRejected(t *testing.T) {
	resetDynamicModelsCache()
	h, secret := keysAuthHandler(t, "", httpauth.Policy{RPM: 1})
	if rec := doAuth(h, http.MethodGet, "/v1/models", secret); rec.Code != http.StatusOK {
		t.Fatalf("首个请求应通过，got %d body=%s", rec.Code, rec.Body)
	}
	rec := doAuth(h, http.MethodGet, "/v1/models", secret)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超 RPM 应 429，got %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("错误码应为 rate_limit_exceeded: %s", rec.Body)
	}
}

// TestMasterKeyBypassesPerKeyPolicy 主口令不受每 key 策略约束（含 RPM）。
func TestMasterKeyBypassesPerKeyPolicy(t *testing.T) {
	resetDynamicModelsCache()
	h, _ := keysAuthHandler(t, "master", httpauth.Policy{RPM: 1})
	for i := 0; i < 3; i++ {
		if rec := doAuth(h, http.MethodGet, "/v1/models", "master"); rec.Code != http.StatusOK {
			t.Fatalf("主口令第 %d 次应通过，got %d body=%s", i+1, rec.Code, rec.Body)
		}
	}
}
