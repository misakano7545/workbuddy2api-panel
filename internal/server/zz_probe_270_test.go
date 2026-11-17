package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/prompt"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestZZProbe270 记录 issue #270 的两条现象在**修复前**的出站形态：
//
//  1. 可拆句 system 的首次拦截请求，三层抢救（split → moved → degraded）与换号共用
//     MaxRotate 名额：默认 3 次里只装得下原文 + split + moved，degraded 整层发不出去，
//     末端以 503 no_healthy_account 收尾（正确的末端应当是 400 content_blocked）；
//  2. degradeGate 是进程级全局的，一个客户端触发后，另一个 system 完全无关、本来不会被拦
//     的客户端，出站前也被改写（两句被拆、单句被替换成中性提示词并把原文搬进 user）。
//
// 现在是「现状记录」而非断言：两者都还没修，写成断言等于把 bug 钉成契约。默认跳过；
// 修完之后应当把 t.Logf 改成断言，并去掉 WB2A_PROBE_DEGRADE 门禁：
//
//	WB2A_PROBE_DEGRADE=1 go test ./internal/server -run TestZZProbe270 -v
func TestZZProbe270(t *testing.T) {
	if os.Getenv("WB2A_PROBE_DEGRADE") == "" {
		t.Skip("set WB2A_PROBE_DEGRADE=1 看降级闸门现状（issue #270）")
	}
	const marker = "FB-MARK-270"

	// 同一套 fake 上游跑两遍，只改 MaxRotate：证明「degraded 层发不出去」是名额问题，
	// 而不是这层不存在。
	for _, maxRotate := range []int{3, 4} {
		t.Run(fmt.Sprintf("MaxRotate=%d", maxRotate), func(t *testing.T) {
			var mu sync.Mutex
			var sent []string

			up := &upstream.Client{
				HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					raw, _ := io.ReadAll(r.Body)
					var req struct {
						Messages []struct {
							Role    string          `json:"role"`
							Content json.RawMessage `json:"content"`
						} `json:"messages"`
					}
					_ = json.Unmarshal(raw, &req)
					var desc []string
					blocked := false
					for _, m := range req.Messages {
						s := strings.TrimSpace(strings.Trim(string(m.Content), `"`))
						if strings.Contains(s, marker) {
							blocked = true
						}
						if strings.Contains(s, prompt.Degraded) {
							desc = append(desc, m.Role+":<中性提示词>")
							continue
						}
						if len(s) > 42 {
							s = s[:42] + "…"
						}
						desc = append(desc, m.Role+":"+s)
					}
					mu.Lock()
					sent = append(sent, fmt.Sprintf("%v", desc))
					mu.Unlock()

					if blocked {
						return &http.Response{
							StatusCode: 400,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(strings.NewReader(`{"code":11-128,"msg":"blocked by security policy"}`)),
						}, nil
					}
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(
							"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")),
					}, nil
				})},
				ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
			}
			p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute,
				PromptMode: "passthrough", MaxRotate: maxRotate})

			call := func(system string) (int, string) {
				body := fmt.Sprintf(`{"model":"glm-5.2","stream":true,"messages":[`+
					`{"role":"system","content":%q},{"role":"user","content":"hi"}]}`, system)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
				return rec.Code, rec.Body.String()
			}
			drain := func() []string {
				mu.Lock()
				defer mu.Unlock()
				out := append([]string(nil), sent...)
				sent = nil
				return out
			}

			// ① 可拆句 system + 每层都带标记：每层都被拦
			code, resp := call(marker + " first sentence. " + marker + " second sentence.")
			calls := drain()
			t.Logf("① 触发请求 状态=%d 出站次数=%d", code, len(calls))
			for i, s := range calls {
				t.Logf("   call %d %s", i+1, s)
			}
			if len(resp) > 160 {
				resp = resp[:160]
			}
			t.Logf("   客户端看到: %s", resp)
			t.Logf("   degrade.Active() = %v", h.degrade.Active())

			// ② 无关客户端，两句 system（本来不会被拦）
			code2, _ := call("Clean other client prompt. Another sentence.")
			calls2 := drain()
			t.Logf("② 无关客户端（两句） 状态=%d 出站次数=%d", code2, len(calls2))
			for i, s := range calls2 {
				t.Logf("   call %d %s", i+1, s)
			}

			// ③ 无关客户端，单句 system（拆不动 → 搬家 + 整体替换）
			code3, _ := call("Single sentence prompt only")
			calls3 := drain()
			t.Logf("③ 无关客户端（单句） 状态=%d 出站次数=%d", code3, len(calls3))
			for i, s := range calls3 {
				t.Logf("   call %d %s", i+1, s)
			}
		})
	}
}
