package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestModelRateLimitStaysModelLevel issue #191 的机制断言：
// 429「model rate limit」只停 **(号, 模型)**，不喂熔断/连败降权、不计 err_total。
//
// 报告里的归因是「429 被当账号故障计进 breaker/degrade，一次请求跨 2~3 个号各记一次
// 失败，再叠加阈值 3 就把健康号打进冷却」。这条用例把机制钉死：两个号、上游恒定返回
// 429 + code:6004，连打三轮，断言
//   - 每个号都有该模型的限额台账（说明惩罚落在模型维度）；
//   - 账号级 until / breaker_until / degrade_until 全为零、err_total 零增长
//     （说明没有任何账号被当成坏号）。
//
// 什么时候该改这条用例：如果将来决定让模型级限流也参与熔断（会把一个模型的额度耗尽
// 放大成整号不可用），那就得先改这里并说明为什么。
func TestModelRateLimitStaysModelLevel(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 429, `{"error":{"code":6004,"message":"该模型使用量超限，将在 10-22 09:40 重置"}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})

	for i := 0; i < 3; i++ {
		body := `{"model":"hy4free","messages":[{"role":"user","content":"hi"}]}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("第 %d 轮 status = %d, want 429（该模型全池停车的本地响应），body=%s",
				i+1, rec.Code, rec.Body)
		}
	}

	for _, st := range p.List() {
		if st.CoolRemaining > 0 || !st.BreakerUntil.IsZero() || !st.DegradeUntil.IsZero() {
			t.Errorf("账号 %s 被当成坏号：cool_remaining=%d breaker_until=%v degrade_until=%v（reason=%q）",
				st.UID, st.CoolRemaining, st.BreakerUntil, st.DegradeUntil, st.Reason)
		}
		if st.ErrTotal != 0 || st.BreakerFails != 0 || st.ConsecutiveFails != 0 {
			t.Errorf("账号 %s 失败计数被动过：err_total=%d breaker_fails=%d consecutive_fails=%d",
				st.UID, st.ErrTotal, st.BreakerFails, st.ConsecutiveFails)
		}
		var hit bool
		for _, m := range st.RateLimitedModels {
			if m.Model == "hy4free" {
				hit = true
			}
		}
		if !hit {
			t.Errorf("账号 %s 应在模型维度留下 hy4free 的限额台账，实际 rate_limited_models=%+v",
				st.UID, st.RateLimitedModels)
		}
	}
	if calls["Bearer at-u1"] == 0 || calls["Bearer at-u2"] == 0 {
		t.Fatalf("两个号都该被尝试过（换号重试仍在发生），实际调用 %v", calls)
	}
}
