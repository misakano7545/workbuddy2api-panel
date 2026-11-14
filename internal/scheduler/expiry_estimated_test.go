package scheduler

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 调度器侧的 upstream -> pool 转换同样要带上 Estimated，否则后台刷新会把面板的
// 「按计量周期推算」标注擦掉。
func TestPoolExpiryBucketsKeepsEstimated(t *testing.T) {
	at := time.Now().Add(48 * time.Hour)
	got := poolExpiryBuckets([]upstream.ExpiryBucket{
		{Date: "2026-11-20", ExpireAt: at, Remain: 5, Estimated: true},
		{Date: "2026-11-21", ExpireAt: at, Remain: 6},
	})
	if len(got) != 2 || !got[0].Estimated || got[1].Estimated {
		t.Fatalf("Estimated 未正确透传：%+v", got)
	}
}
