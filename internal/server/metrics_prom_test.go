package server

import (
	"math"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 指标口径：pool_state{state="disabled"} = 禁用 + 暂停选号（吸收上游 PR #130 时 RealmHealth
// 把两者拆开计数，这里必须显式合并，否则仪表盘上的「不可用」会莫名少掉暂停号）。
func TestPromPoolStateMergesPausedIntoDisabled(t *testing.T) {
	h := pool.RealmHealth{Disabled: 2, Paused: 3}
	if got := promPoolStateValue(h, "disabled"); got != 5 {
		t.Errorf("pool_state disabled=%d want 5（禁用 2 + 暂停 3）", got)
	}
	// 其它状态位不受影响。
	if got := promPoolStateValue(pool.RealmHealth{Disabled: 2, Paused: 3, Healthy: 7}, "healthy"); got != 7 {
		t.Errorf("healthy=%d want 7", got)
	}
}

// 指标平均速率口径：非流式行（无 TTFB 观测）既不进分母也不进分子——端到端耗时含
// 预填，混进去会把平均速率算成一个不可比的低值/虚高值（tokensPerSecond 的同一条规则）。
func TestPromTokensPerSecSkipsRowsWithoutTTFB(t *testing.T) {
	promMetrics.mu.Lock()
	promMetrics.by = map[string]*promAcc{}
	promMetrics.mu.Unlock()
	t.Cleanup(func() {
		promMetrics.mu.Lock()
		promMetrics.by = map[string]*promAcc{}
		promMetrics.mu.Unlock()
	})
	// 流式行：200ms TTFB、2s 总时长、100 token → 生成段 1.8s
	noteProm(&chatStat{model: "m", mode: "stream", status: 200, toks: 100, ttfb: 200 * time.Millisecond}, 2*time.Second)
	// 非流式行：60s 墙钟（含预填）、1000 token → 必须被速率口径忽略
	noteProm(&chatStat{model: "m", mode: "sync", status: 200, toks: 1000}, 60*time.Second)

	got := MetricsSnapshotOf().Models[0].TokensPerSec
	if math.Abs(got-55.56) > 0.1 {
		t.Errorf("TokensPerSec=%.2f want 55.56（100 token / 1.8s；非流式行不得进分子分母）", got)
	}
	if p := MetricsSnapshotOf().Models[0]; p.CompletionTokens != 1100 {
		t.Errorf("token 计数仍应含非流式行：got %d want 1100", p.CompletionTokens)
	}
}
