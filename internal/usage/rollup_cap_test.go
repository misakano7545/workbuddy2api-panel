package usage

import (
	"fmt"
	"testing"
	"time"
)

// TestRollupEnforcesBucketCap 桶数硬上限此前是空转：Start 超限时调的 Rollup 只折 90 天
// 前的小时桶，异常流量下新桶全在保留窗口内 → 一个也折不掉。现在超限时从最旧的小时桶
// 逐个折，直到回到上限内；折叠无损（请求数不变），到龄折叠的细粒度口径不变。
func TestRollupEnforcesBucketCap(t *testing.T) {
	old := maxBuckets
	maxBuckets = 10
	defer func() { maxBuckets = old }()

	r := New("")
	now := time.Now()
	for m := 0; m < 3; m++ {
		for h := 0; h < 10; h++ { // 3 个模型 × 近 10 小时，全在 hourlyKeep 窗口内
			r.Add(now.Add(-time.Duration(h)*time.Hour), "cn", "u1", fmt.Sprintf("m%d", m),
				Delta{PromptTokens: 1, HasPromptTokens: true}, true)
		}
	}
	if got := len(r.buckets); got != 30 {
		t.Fatalf("前置条件：应累积 30 个小时桶，实际 %d", got)
	}

	r.Rollup(now)
	if got := len(r.buckets); got > maxBuckets {
		t.Fatalf("桶数硬上限未生效：%d 桶（上限 %d），守卫空转", got, maxBuckets)
	}
	if got := r.Snapshot(0, nil).Totals.Requests; got != 30 {
		t.Fatalf("折叠必须无损：请求数 = %d, want 30", got)
	}
	// 幂等：再折一次不应改变总量，也不应超上限。
	r.Rollup(now)
	if got := len(r.buckets); got > maxBuckets {
		t.Fatalf("二次折叠仍超上限：%d", got)
	}
	if got := r.Snapshot(0, nil).Totals.Requests; got != 30 {
		t.Fatalf("二次折叠后请求数 = %d, want 30（幂等被破坏）", got)
	}
}
