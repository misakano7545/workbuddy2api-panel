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

// clientKeysAuthHandler 构造带客户端密钥表的最小 Handler：管理员密钥与密钥表都
// 可指定，用来覆盖「表里建过密钥就必须带有效密钥」的准入矩阵。
func clientKeysAuthHandler(t *testing.T, apiKey string, inputs ...httpauth.ClientKeyInput) (*Handler, *httpauth.ClientKeyStore, []httpauth.ClientKey) {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":200000,"maxOutputTokens":32000}]}}`, false
	})
	store, err := httpauth.OpenClientKeys("")
	if err != nil {
		t.Fatalf("OpenClientKeys: %v", err)
	}
	keys := make([]httpauth.ClientKey, 0, len(inputs))
	for i := range inputs {
		inputs[i].Enabled = true
		k, err := store.Create(inputs[i])
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		keys = append(keys, k)
	}
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1"}),
		Upstream:   up,
		APIKey:     apiKey,
		ClientKeys: store,
	})
	return h, store, keys
}

// TestClientKeyAdmitsModels 客户端密钥能调 /v1/*：200 且计一次成功请求。
func TestClientKeyAdmitsModels(t *testing.T) {
	resetDynamicModelsCache()
	h, store, keys := clientKeysAuthHandler(t, "", httpauth.ClientKeyInput{Name: "team"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("建过客户端密钥后无凭据必须 401，got %d body=%s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keys[0].Secret)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("有效客户端密钥应放行，got %d body=%s", rec.Code, rec.Body)
	}
	got, ok := store.Get(keys[0].ID)
	if !ok || got.Used.Requests != 1 {
		t.Fatalf("成功请求应计 1 次，got %+v", got)
	}
}

// TestClientKeyXAPIKeyCarrier Anthropic 侧 x-api-key 携带方式同样走客户端密钥表。
func TestClientKeyXAPIKeyCarrier(t *testing.T) {
	resetDynamicModelsCache()
	h, _, keys := clientKeysAuthHandler(t, "", httpauth.ClientKeyInput{Name: "team"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", keys[0].Secret)
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key 携带的客户端密钥应放行，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestClientKeyRejectsOpsEndpoints 运维端点只认管理员密钥：客户端密钥 403
// admin_key_required，且准入预扣要退还（被拒的请求不计量）。
func TestClientKeyRejectsOpsEndpoints(t *testing.T) {
	h, store, keys := clientKeysAuthHandler(t, "admin-secret", httpauth.ClientKeyInput{Name: "team"})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+keys[0].Secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("客户端密钥调 /status 应 403，got %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "admin_key_required") {
		t.Fatalf("403 应带 admin_key_required，got %s", rec.Body)
	}
	if got, _ := store.Get(keys[0].ID); got.Used.Requests != 0 || got.Today.Requests != 0 {
		t.Fatalf("被拒请求不该计量，got used=%d today=%d", got.Used.Requests, got.Today.Requests)
	}

	// 管理员密钥仍然能调运维端点（不计量）。
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员密钥应放行 /status，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestClientKeyQuotaExceeded 日限用完：429 key_quota_exceeded + Retry-After。
func TestClientKeyQuotaExceeded(t *testing.T) {
	resetDynamicModelsCache()
	h, _, keys := clientKeysAuthHandler(t, "", httpauth.ClientKeyInput{
		Name:       "team",
		DailyLimit: httpauth.Limits{Requests: 1},
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+keys[0].Secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("第一次应放行，got %d body=%s", rec.Code, rec.Body)
	}
	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超额应 429，got %d body=%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("日限 429 必须带 Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "key_quota_exceeded") {
		t.Fatalf("429 应带 key_quota_exceeded，got %s", rec.Body)
	}
}

// TestClientKeyDisabledExpired 停用/过期都 401，码分开（面板/客户端据此区分）。
func TestClientKeyDisabledExpired(t *testing.T) {
	h, store, keys := clientKeysAuthHandler(t, "", httpauth.ClientKeyInput{Name: "team"})
	if _, err := store.Update(keys[0].ID, httpauth.ClientKeyInput{Name: "team", Enabled: false}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keys[0].Secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "key_disabled") {
		t.Fatalf("停用应 401 key_disabled，got %d body=%s", rec.Code, rec.Body)
	}

	h2, _, keys2 := clientKeysAuthHandler(t, "", httpauth.ClientKeyInput{
		Name:      "old",
		ExpiresAt: time.Now().Add(-time.Hour),
	})
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keys2[0].Secret)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "key_expired") {
		t.Fatalf("过期应 401 key_expired，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestAdminKeyStillBypassesClientKeys 管理员密钥与客户端密钥并存时管理员不计量：
// 表里有密钥也不影响管理员密钥调 /v1/*。
func TestAdminKeyStillBypassesClientKeys(t *testing.T) {
	resetDynamicModelsCache()
	h, store, _ := clientKeysAuthHandler(t, "admin-secret", httpauth.ClientKeyInput{Name: "team"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员密钥应放行，got %d body=%s", rec.Code, rec.Body)
	}
	for _, k := range store.List() {
		if k.Used.Requests != 0 || k.Today.Requests != 0 {
			t.Fatalf("管理员流量不该计入客户端密钥: %+v", k)
		}
	}
}
