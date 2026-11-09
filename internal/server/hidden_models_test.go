// hidden_models_test.go config models.hidden（模型黑名单）的生效面回归。
//
// 契约三条，缺一都算没实现：
//  1. GET /v1/models 不列（cn 与 global 两域、以及单列的出图模型）；
//  2. 点名调用直接拒绝（chat 与出图两条入口）——只藏列表不够，知道名字仍可硬调，
//     而且硬调会真的打上游、真的花积分；
//  3. 面板保存后热生效（livecfg 快照优先于启动配置），不需要重启进程。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
)

// hiddenCatalog 一份含对话模型 + 出图模型的 CN 目录答复。
func hiddenCatalog() (int, string, bool) {
	return 200, `{"code":0,"data":{"models":[` +
		`{"id":"glm-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
		`{"id":"kimi-k3-1","maxInputTokens":100000,"maxOutputTokens":8192},` +
		`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]}],` +
		`"agents":[{"name":"cli","models":["glm-5.2","kimi-k3-1"]}]}}`, false
}

func resetDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// 列表过滤：裸名藏两域中的当前域，cn: 前缀只藏该域；出图模型同样受管。
func TestModelsHiddenFiltersList(t *testing.T) {
	resetDynamicModelsCache()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return hiddenCatalog() })
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     up,
		ModelsHidden: []string{"glm-5.2", "cn:hunyuan-image-alpha"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	seen := map[string]bool{}
	for _, m := range resp.Data {
		seen[m["id"].(string)] = true
	}
	for _, id := range []string{"cn:glm-5.2", "cn:hunyuan-image-alpha"} {
		if seen[id] {
			t.Errorf("隐藏的模型不该出现在 /v1/models：%s（全部：%v）", id, seen)
		}
	}
	if !seen["cn:kimi-k3-1"] {
		t.Errorf("未隐藏的模型必须仍在列表里：%v", seen)
	}
}

// 点名拒绝：打上游前就 400，且给出可操作的 hint（指引换模型而不是重试）。
func TestModelsHiddenRejectsChatBeforeUpstream(t *testing.T) {
	resetDynamicModelsCache()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("隐藏模型的请求不该打到上游（会真花积分）")
		return 500, "", false
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     up,
		ModelsHidden: []string{"glm-5.2"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s（want 400 model_unavailable）", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "model_unavailable") {
		t.Errorf("应报 code=model_unavailable：%s", body)
	}
	if !strings.Contains(body, "hidden") {
		t.Errorf("文案应说明是网关配置隐藏：%s", body)
	}
}

// 带 realm 前缀的点名同样被拒（"cn:glm-5.2" 与裸名等价）。
func TestModelsHiddenRejectsRealmPrefixedChat(t *testing.T) {
	resetDynamicModelsCache()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("带前缀的隐藏模型请求不该打到上游")
		return 500, "", false
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     up,
		ModelsHidden: []string{"glm-5.2"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// 黑名单按域生效：隐藏 cn:glm-5.2 不影响 global:glm-5.2（同一 id 在两域是不同模型）。
func TestModelsHiddenScopeIsPerRealm(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), ModelsHidden: []string{"cn:glm-5.2"}})
	if !h.hiddenModel("cn", "glm-5.2") {
		t.Error("cn:glm-5.2 应被隐藏")
	}
	if h.hiddenModel("global", "glm-5.2") {
		t.Error("cn: 前缀不该影响 global 域")
	}
	// 裸名条目两域都藏。
	h2 := NewHandler(Config{Pool: testPoolWith(), ModelsHidden: []string{"glm-5.2"}})
	if !h2.hiddenModel("cn", "glm-5.2") || !h2.hiddenModel("global", "glm-5.2") {
		t.Error("裸名应两域都藏")
	}
	// 空模型名不隐藏（避免防御路径把「没写 model」误判成隐藏）。
	if h.hiddenModel("cn", "") {
		t.Error("空模型名不该判为隐藏")
	}
}

// 热生效：面板保存后替换 livecfg 快照，无需重启即生效（也验证快照优先于启动配置）。
func TestModelsHiddenHotReloadViaLive(t *testing.T) {
	resetDynamicModelsCache()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return hiddenCatalog() })
	live := livecfg.New(livecfg.Snapshot{HiddenModels: livecfg.NormalizeHiddenModels(nil)})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     up,
		ModelsHidden: nil, // 启动配置为空，隐藏完全靠运行期快照
		Live:         live,
	})
	if h.hiddenModel("cn", "glm-5.2") {
		t.Fatal("快照为空时不该隐藏")
	}
	live.Store(livecfg.Snapshot{HiddenModels: livecfg.NormalizeHiddenModels([]string{"glm-5.2"})})
	if !h.hiddenModel("cn", "glm-5.2") {
		t.Fatal("替换快照后应立刻生效（面板保存即热生效）")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("热生效后点名应被拒：code=%d body=%s", rec.Code, rec.Body)
	}
}

// 出图入口同样挡：列表里藏掉的图片模型不能还能直接生成（否则「隐藏」只是遮住菜单）。
func TestModelsHiddenRejectsImages(t *testing.T) {
	fake := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{
		Pool:         imagePool(),
		Upstream:     fake.client(),
		ModelsHidden: []string{"cn:hunyuan-image-alpha"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"一只猫"}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s（want 400）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Errorf("应报 model_unavailable：%s", rec.Body)
	}
	if len(fake.paths) != 0 {
		t.Errorf("隐藏模型不该打到上游：%v", fake.paths)
	}
}

// 出图模型列表同样过滤（generationModelEntries 单列的那批）。
func TestModelsHiddenFiltersGenerationEntries(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return hiddenCatalog() })
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     up,
		ModelsHidden: []string{"hunyuan-image-alpha"},
	})
	// 触发一次目录探测，让出图模型进入旁路留存。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	for _, e := range h.generationModelEntries() {
		if strings.Contains(e["id"].(string), "hunyuan-image-alpha") {
			t.Fatalf("隐藏的出图模型不该出现在列表：%v", e)
		}
	}
}
