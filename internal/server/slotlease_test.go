// slotlease_test.go 在途名额（Pool.Acquire/Release）的释放不变式。
//
// 为什么单独钉这个：hub PR #813 修的就是这一类——错误分支直接 raise 跳过了循环后的
// 统一释放，账号的在途计数永久 +1；`account_max_concurrent=1` 下漏一个名额就足以让该
// 账号永远「已在服务 1 个请求」，面板显示健康、账号测试也通过，但派发再也不选它，
// 只有重启进程才恢复。本仓的释放是函数出口的 defer（handler.go 的 heldUID），结构上
// 覆盖所有 return 与 panic；这组用例把它钉住，防止以后有人把 defer 挪回「成功路径末尾」。
//
// 11140 命中会禁用账号，第二次请求本就无号可用（见 restrict_test.go），故不在此列。
package server

import (
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestInFlightLeaseReleasedOnSuccess 成功路径的释放只由出口 defer 负责（fail() 不参与），
// 所以在途上限=1 时连着两个成功请求必须都成功：撤掉 defer 后第二个请求会抢不到名额。
func TestInFlightLeaseReleasedOnSuccess(t *testing.T) {
	const okBody = `data: {"id":"cmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}` + "\n\n" +
		`data: {"id":"cmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, okBody, true })
	p := testPoolWith(&auth.Auth{UID: "s1", AccessToken: "at-s1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up})

	for i := 1; i <= 2; i++ {
		rec := postChatRestricted(h)
		if rec.Code != 200 {
			t.Fatalf("第 %d 个成功请求 code=%d want 200，body=%s（上一次的名额没还 → 无号可用）", i, rec.Code, rec.Body)
		}
		for _, st := range p.List() {
			if st.InFlight != 0 {
				t.Fatalf("第 %d 次请求后 uid=%s 在途=%d want 0", i, st.UID, st.InFlight)
			}
		}
	}
}

// TestInFlightLeaseReleasedOnErrorPaths 上游一直回错误、在途上限=1 时，每个请求都必须
// 把名额还回去：否则后续请求会拿不到号（no_healthy_account），而不是继续打上游。
func TestInFlightLeaseReleasedOnErrorPaths(t *testing.T) {
	// 三种都走「选号成功后才失败」的分支：内容级 4xx（ErrClient）、账号级 4xx
	// （ErrAccountFault）、上游 5xx。它们都不禁用账号，所以能反复打到同一个号。
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"client 4xx", 400, `{"code":10100,"msg":"bad request","requestId":"aaaabbbbccccdddd"}`},
		{"account 4xx", 400, `{"code":14017,"msg":"trial not activated","requestId":"aaaabbbbccccdddd"}`},
		{"upstream 5xx", 502, `upstream unavailable`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				calls++
				return c.status, c.body, false
			})
			p := testPoolWith(&auth.Auth{UID: "q1", AccessToken: "at-q1", ExpiresAt: 9999999999})
			p.SetMaxInFlight(1) // 漏一个名额立刻致命：下一个请求会拿不到号
			h := NewHandler(Config{Pool: p, Upstream: up})

			for i := 1; i <= 2; i++ {
				if rec := postChatRestricted(h); rec.Code == 0 {
					t.Fatal("no response")
				}
				// 名额必须已归还：留在条目上就是 hub #813 的永久占用。
				for _, st := range p.List() {
					if st.InFlight != 0 {
						t.Fatalf("第 %d 次请求后 uid=%s 在途=%d want 0（名额泄漏会让该账号再也接不到单）", i, st.UID, st.InFlight)
					}
				}
				if calls != i {
					t.Fatalf("第 %d 次请求应该打到上游（calls=%d）；拿不到号说明上一次的名额没还", i, calls)
				}
			}
		})
	}
}
