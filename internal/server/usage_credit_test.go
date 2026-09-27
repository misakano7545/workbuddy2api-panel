package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// 积分进入用量记录的端到端链路：上游 usage.credit → 记录器桶 → 面板聚合。
// 只测 usage 包是不够的——SSE 末帧解析与非流式 usage 解析各是一条独立入口，
// 少接一处，面板上就会出现「token 有数、积分恒 0」。
func TestUsageRecordsCreditsFromUpstream(t *testing.T) {
	const credit = 2.5
	// 流式：末帧 usage 带 credit。
	sse := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\"," +
		"\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\"," +
		"\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
		"\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"credit\":2.5}}\n\n" +
		"data: [DONE]\n\n"
	// 非流式：客户端要 stream:false 时网关聚合 SSE，走 usageDeltaFromResponse。
	// 上游出站始终是 stream:true（强制），所以两个 case 的 fake 都返回 SSE——
	// 差别只在**客户端**要的是流式还是聚合。
	jsonBody := sse

	for _, tc := range []struct {
		name           string
		body           string
		stream         bool
		wantCredit     float64
		wantTotalToken int64
	}{
		{"流式", sse, true, credit, 2},
		{"非流式", jsonBody, false, credit, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := usage.New("")
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, tc.body, tc.stream })
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
				Usage:    rec,
			})
			req := httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":`+boolLit(tc.stream)+`}`))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			st := rec.Snapshot(24, nil)
			if st.Totals.Credits != tc.wantCredit {
				t.Errorf("积分 = %v, want %v（上游 credit 没进用量桶）", st.Totals.Credits, tc.wantCredit)
			}
			if st.Totals.TotalTokens != tc.wantTotalToken {
				t.Errorf("token = %d, want %d", st.Totals.TotalTokens, tc.wantTotalToken)
			}
		})
	}
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
