package server

import (
	"testing"

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
