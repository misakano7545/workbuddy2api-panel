// usage_attribution_test.go 钉住「一次尝试的用量/成本/冷却记在哪个模型名下」这条口径。
//
// 背景（用户报告）：模型限流映射页出现「池级配额 600M、已用 941.9M · 157.0%」这种
// 超额。配额是上游按「账号 × 模型」发的，正常情况下已用不可能超过池级配额；根因是
// 降级（6004 换等价模型、本地限流降级）之后记账仍写**请求的原模型**，于是原模型
// 虚高、兜底模型虚低，成本账本、倍率维度、冷却台账跟着一起错。这里从三条可见出口
// 一起钉住：
//   - usage 分桶 / ModelWindow（模型限流映射页的额度来源）
//   - pool 成本账本 ModelCosts、缓存告警键、CreditByModel 的倍率维度（选号依据）
//   - RateLimitedModels（6004 冷却台账，下一条请求据此避开模型）
package server

import (
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

// okChatSSECredit 带 usage/credit/前缀缓存字段的成功流：一次成功尝试就同时喂到用量
// 分桶、倍率维度、成本账本与缓存告警四条路径。
const okChatSSECredit = `data: {"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"credit":0.42,"prompt_cache_hit_tokens":90,"prompt_cache_miss_tokens":10}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestUsageAttributionFollowsServedModel 6004 降级到等价模型后，用量/倍率/成本/缓存告警
// 必须记在**实际出站**的模型名下；记到请求的原模型上就是限流页超额的直接来源。
func TestUsageAttributionFollowsServedModel(t *testing.T) {
	oldWarn := cacheMissWarn
	cacheMissWarn = &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}
	t.Cleanup(func() { cacheMissWarn = oldWarn })

	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{429, rateLimit6004Body})
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSECredit})

	client := up.client()
	// 两个模型倍率不同：记账若按原模型取倍率，倍率维度会留下 0.03。
	client.StoreModelRatesForTest("cn", map[string]string{
		"deepseek-v4.1-flash": "0.03",
		"deepseek-v4-flash":   "0.01",
	})
	rec := usage.New("")
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999}),
		Upstream:      client,
		MaxRotate:     3,
		ModelFallback: func() map[string]string { return map[string]string{"deepseek-v4.1-flash": "deepseek-v4-flash"} },
		Usage:         rec,
	})

	if resp := doFallbackChat(h, "deepseek-v4.1-flash"); resp.Code != 200 {
		t.Fatalf("status=%d want 200（降级后应成功）, body=%s", resp.Code, resp.Body)
	}
	if got := up.seen(); len(got) != 2 || got[0] != "deepseek-v4.1-flash" || got[1] != "deepseek-v4-flash" {
		t.Fatalf("上游收到 %v，want [deepseek-v4.1-flash deepseek-v4-flash]", got)
	}

	// 一、用量分桶（限流页额度）：token 只能落在服务模型上。
	rows := rec.ModelWindow(usage.Window{})
	if len(rows) == 0 {
		t.Fatal("用量分桶为空：recordAttempt 没跑？")
	}
	servedTokens := int64(0)
	for _, r := range rows {
		if r.Model == "deepseek-v4-flash" {
			servedTokens += r.Tokens
			continue
		}
		if r.Tokens != 0 {
			t.Fatalf("用量记到了非服务模型 %s：%+v（应记在 deepseek-v4-flash）", r.Model, r)
		}
	}
	if servedTokens != 105 {
		t.Fatalf("服务模型 token=%d want 105（rows=%+v）", servedTokens, rows)
	}

	// 二、倍率维度：跟着服务模型（0.01），不是原模型的 0.03。
	snap := rec.SnapshotWindow(usage.Window{}, nil, nil)
	servedKey := ""
	for _, row := range snap.CreditByModel {
		if row.Rate == "0.03" {
			t.Fatalf("倍率维度用了原模型的 0.03：%+v", row)
		}
		if row.Rate == "0.01" {
			servedKey = row.Key
		}
	}
	if servedKey == "" {
		t.Fatalf("倍率维度缺服务模型 0.01 一行：%+v", snap.CreditByModel)
	}
	if !strings.HasSuffix(servedKey, "deepseek-v4-flash") {
		t.Fatalf("倍率维度行 key=%q，want 服务模型 deepseek-v4-flash", servedKey)
	}

	// 三、成本账本（选号依据）：实测单价记在服务模型名下。
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("账号从池里消失")
	}
	if len(st.ModelCosts) != 1 || st.ModelCosts[0].Model != "deepseek-v4-flash" {
		t.Fatalf("成本账本记在错模型：%+v（应只有 deepseek-v4-flash）", st.ModelCosts)
	}

	// 四、缓存告警信号：键是服务模型，原模型不该被当成「曾命中」。
	if !cacheMissWarn.everHit["deepseek-v4-flash"] {
		t.Fatal("缓存告警没按服务模型建键")
	}
	if cacheMissWarn.everHit["deepseek-v4.1-flash"] {
		t.Fatal("缓存告警把请求原模型当成了服务模型")
	}
}

// TestCooldownAttributionFollowsServedModel 两级 6004 降级：第二跳撞墙的是**等价模型**，
// 冷却必须记在它名下。记成原模型（老实现）会把已经放弃的模型反复记一遍，而真正被
// 限流的等价模型一直显示健康——下一条同类请求立刻再撞一次 6004，降级等于白做。
func TestCooldownAttributionFollowsServedModel(t *testing.T) {
	up := &modelAwareUpstream{}
	for i := 0; i < 2; i++ {
		up.reply = append(up.reply, struct {
			status int
			body   string
		}{429, rateLimit6004Body})
	}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})

	h := newFallbackHandler(map[string]string{
		"deepseek-v4.1-flash": "deepseek-v4-flash",
		"deepseek-v4-flash":   "deepseek-v4-flash-sg",
	}, up.client())

	if resp := doFallbackChat(h, "deepseek-v4.1-flash"); resp.Code != 200 {
		t.Fatalf("status=%d want 200, body=%s", resp.Code, resp.Body)
	}
	got := up.seen()
	if len(got) != 3 || got[1] != "deepseek-v4-flash" || got[2] != "deepseek-v4-flash-sg" {
		t.Fatalf("上游收到 %v，want [deepseek-v4.1-flash deepseek-v4-flash deepseek-v4-flash-sg]", got)
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("账号从池里消失")
	}
	limited := map[string]bool{}
	for _, row := range st.RateLimitedModels {
		limited[row.Model] = true
	}
	for _, want := range []string{"deepseek-v4.1-flash", "deepseek-v4-flash"} {
		if !limited[want] {
			t.Fatalf("6004 冷却台账缺 %s：%+v（第二跳被限流的是等价模型）", want, st.RateLimitedModels)
		}
	}
}
