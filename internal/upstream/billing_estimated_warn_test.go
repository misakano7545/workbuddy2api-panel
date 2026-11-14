package upstream

import (
	"bytes"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 限频判定本身：间隔内只放行一次、过了间隔再放行、账号之间互不影响，
// 并发下同一间隔也只有一个调用方拿到 true。
func TestEstimatedExpiryWarnDue(t *testing.T) {
	const uidA, uidB = "warn-due-a", "warn-due-b"
	estimatedExpiryWarned.Delete(uidA)
	estimatedExpiryWarned.Delete(uidB)
	t0 := time.Now()
	if !estimatedExpiryWarnDue(uidA, t0) {
		t.Fatal("首次应放行")
	}
	if estimatedExpiryWarnDue(uidA, t0.Add(estimatedExpiryWarnEvery-time.Second)) {
		t.Fatal("间隔内不应再放行")
	}
	if !estimatedExpiryWarnDue(uidB, t0) {
		t.Fatal("另一账号应独立放行")
	}
	t1 := t0.Add(estimatedExpiryWarnEvery)
	if !estimatedExpiryWarnDue(uidA, t1) {
		t.Fatal("过了间隔应再次放行")
	}
	if estimatedExpiryWarnDue(uidA, t1.Add(time.Second)) {
		t.Fatal("再次放行后应重新计时")
	}

	estimatedExpiryWarned.Delete(uidA)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if estimatedExpiryWarnDue(uidA, t0) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("并发下应只放行 1 次，实际 %d", wins.Load())
	}
}

// 「到期日只能推算」的 WARN 每个账号只在间隔内写一次：余额每 5 分钟刷新一轮，
// 不限频就是每号每天 288 行同样的日志。
func TestEstimatedExpiryWarnRateLimitedPerAccount(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	estimatedExpiryWarned.Delete(globalAuth().UID)

	rows := []expiryRow{{code: "PKG-OLD", end: dayAt(3), remain: 20, deduct: false, total: 30}}
	for i := 0; i < 3; i++ {
		snapshotWithRows(t, rows, 0)
	}
	if n := strings.Count(buf.String(), "只能按计量周期"); n != 1 {
		t.Fatalf("同一账号间隔内应只写 1 行 WARN，实际 %d 行：%s", n, buf.String())
	}
}
