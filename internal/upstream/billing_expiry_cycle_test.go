// 国际版到期批次的**按周期**聚合回归（issue #200）。
//
// 现场（2026-11-10，v2026.11.10.2032 线上实测，账号 misakano7545）：summary 报
// remain=1027，明细 32 行**同一个 PackageCode**（每天发一个 Bonus Pack，各自
// CycleEndTime 逐日递推、各带自己的 CycleCapacityRemain），旧代码按码归并只留
// 「最早的未来到期」那条，再把 summary 里当前周期的剩余配上去，于是面板显示：
//
//	2026-11-11（明天到期）remain=927   ← 927 分全挂到明天
//	2034-12-23            remain=100
//
// 而真实分布是「明天 27、后天 30、……逐日到 12-10 各 30」。更严重的是 expiring
// （7 天内快过期额度）也被算成 927，池子会把该号当成「积分即将作废」优先消耗。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// expiryFixture 用一组「同码多周期」明细起一个假上游，返回快照结果。
type expiryRow struct {
	code    string
	end     time.Time
	remain  float64
	deduct  bool // true 时给 DeductionEndTime（真实扣费截止），false 只给 CycleEndTime
	status  int
	total   float64
	summary *float64 // 非 nil 时该码在 summary 里的当前周期剩余
}

func snapshotWithRows(t *testing.T, rows []expiryRow, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, buckets []ExpiryBucket) {
	t.Helper()
	// summary：同一个码只列一次（模拟上游真机形态）。默认按「该码各周期自报剩余之和」
	// 合成——线上实测就是这么对的（32 行同码 remain 之和 = summary remain）。
	// expiryRow.summary 非 nil 时用显式值覆盖，用来构造「明细为 0 但 summary 有数」。
	type sumRow struct {
		code   string
		total  float64
		remain float64
	}
	var sums []sumRow
	idx := map[string]int{}
	for _, r := range rows {
		i, ok := idx[r.code]
		if !ok {
			i = len(sums)
			idx[r.code] = i
			sums = append(sums, sumRow{code: r.code})
		}
		sums[i].total += r.total
		sums[i].remain += r.remain
	}
	for _, r := range rows {
		if r.summary != nil {
			sums[idx[r.code]].remain = *r.summary
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
			var parts []string
			for _, s := range sums {
				parts = append(parts, fmt.Sprintf(
					`{"PackageCode":%q,"CycleTotalCapacity":"%.2f","CycleRemainCapacity":"%.2f"}`,
					s.code, s.total, s.remain))
			}
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Packages":[` + strings.Join(parts, ",") + `]}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-free-packages"):
			var parts []string
			for _, r := range rows {
				ded := int64(0)
				expired := ""
				if r.deduct {
					ded = r.end.UnixMilli()
				} else {
					expired = "" // 只给 CycleEndTime，走推算回退
				}
				parts = append(parts, fmt.Sprintf(
					`{"PackageCode":%q,"PackageName":"Bonus Pack","Status":%d,`+
						`"CycleCapacitySize":%.2f,"CycleCapacityRemain":%.2f,"CycleCapacityRemainPrecise":%.2f,`+
						`"CycleEndTime":%q,"ExpiredTime":%q,"DeductionEndTime":%d}`,
					r.code, r.status, r.total, r.remain, r.remain,
					r.end.In(softRateResetLoc).Format("2006-01-02 15:04:05"), expired, ded))
			}
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[` + strings.Join(parts, ",") + `],"TotalCount":` +
				fmt.Sprint(len(rows)) + `}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-paid-packages"):
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[],"TotalCount":0}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err := c.UserResourceSnapshot(globalAuth(), soon)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return remain, total, expiring, earliestAt, earliestRemaining, buckets
}

func dayAt(days int) time.Time {
	base := time.Now().In(softRateResetLoc)
	// 取当日 00:00 而不是 12:00：到期窗口是按「now + N 小时」判的，固定 12:00 会让
	// 「第 7 天算不算在窗口内」随时钟漂移（上午跑会少算一天、下午跑才对），
	// 00:00 对任何时刻都稳定落在窗口内，第 8 天则稳定落在窗口外。
	return time.Date(base.Year(), base.Month(), base.Day()+days, 0, 0, 0, 0, softRateResetLoc)
}

var f64 = func(v float64) *float64 { return &v }

// issue #200 的原样场景：同一个 PackageCode 三条明细（一条已用完的旧周期 +
// 两条还有余额的新周期）。旧代码会把 summary 的 59 全挂到最早的 11-11 上。
//
// 变异判据：把归并键改回只按 PackageCode（取最早 end + 借 summary 余额），
// 本用例会在「11-11 出现了一个 59 的桶」上红。
func TestGlobalExpiryKeepsCyclesSeparate(t *testing.T) {
	rows := []expiryRow{
		// 旧周期：明天到期但已用完。
		{code: "PKG-BONUS", end: dayAt(1), remain: 0, deduct: true, total: 30},
		// 两个仍有效的周期，各自带自己的剩余。
		{code: "PKG-BONUS", end: dayAt(29), remain: 29, deduct: true, total: 30},
		{code: "PKG-BONUS", end: dayAt(30), remain: 30, deduct: true, total: 30},
	}
	_, _, expiring, earliestAt, earliestRem, buckets := snapshotWithRows(t, rows, 0)

	if len(buckets) != 2 {
		t.Fatalf("应只有 2 个未来批次，实际 %d：%+v", len(buckets), buckets)
	}
	if buckets[0].Date != dayAt(29).Format("2006-01-02") || buckets[0].Remain != 29 {
		t.Fatalf("第一批 = %s/%d，want %s/29（明细自报剩余，不借 summary）",
			buckets[0].Date, buckets[0].Remain, dayAt(29).Format("2006-01-02"))
	}
	if buckets[1].Remain != 30 {
		t.Fatalf("第二批 remain=%d want 30", buckets[1].Remain)
	}
	if got, want := earliestAt.Format("2006-01-02"), dayAt(29).Format("2006-01-02"); got != want {
		t.Fatalf("earliestAt=%s want %s（已用完的旧周期不得拉早最早到期日）", got, want)
	}
	if earliestRem != 29 {
		t.Fatalf("earliestRemaining=%d want 29", earliestRem)
	}
	if expiring != 0 {
		t.Fatalf("expiring=%d want 0（快过期窗口关了，且旧周期已用完）", expiring)
	}
}

// 真机形态：同码 31 个逐日周期 + 一个长期包。断言「按周期各算各的」，
// 且逐日桶之和等于各周期自报剩余之和（不会重复计入，也不会堆到一天）。
func TestGlobalExpiryDailyBonusPacks(t *testing.T) {
	var rows []expiryRow
	sumCap := 0.0
	for d := 1; d <= 31; d++ {
		r := 30.0
		if d == 1 {
			r = 27 // 只用了 3 分
		}
		sumCap += r
		rows = append(rows, expiryRow{code: "PKG-BONUS", end: dayAt(d), remain: r, deduct: true, total: 30})
	}
	// 长期包（2034 到期，100 分）。
	rows = append(rows, expiryRow{code: "PKG-LONG", end: time.Date(2034, 12, 23, 23, 25, 0, 0, softRateResetLoc),
		remain: 100, deduct: true, total: 100})

	// 「7 天内到期」的窗口必须锚到日界，不能用固定 168h。
	//
	// dayAt(d) 落在每天 12:00 CST，而固定 168h 是「现在 + 7×24h」：D+7 的 12:00 是否
	// 落在窗口内，取决于此刻在 CST 的哪个时段——12:00 之后跑能命中 7 批（207），
	// 12:00 之前跑只能命中 6 批（177）。这个用例因此在每天 00:00–12:00 CST 必红。
	// 锚到 dayAt(7) 之后，无论几点跑都恰好覆盖 D+1..D+7，断言 207 恒成立。
	soon := dayAt(7).Sub(time.Now())
	remain, total, expiring, earliestAt, earliestRem, buckets := snapshotWithRows(t, rows, soon)

	if remain != int64(sumCap+100) || total != int64(30*31+100) {
		t.Fatalf("remain/total = %d/%d want %d/%d", remain, total, int64(sumCap+100), int64(30*31+100))
	}
	if len(buckets) != 32 {
		t.Fatalf("应有 31 个逐日桶 + 1 个长期桶 = 32，实际 %d", len(buckets))
	}
	sum := int64(0)
	for _, b := range buckets {
		sum += b.Remain
	}
	if sum != int64(sumCap+100) {
		t.Fatalf("逐日桶之和 = %d，与 remain %d 不符（重复计入或漏计）", sum, int64(sumCap+100))
	}
	if buckets[0].Remain != 27 {
		t.Fatalf("最早一批 remain=%d want 27（明天只到期 27 分，不是全部）", buckets[0].Remain)
	}
	if got, want := earliestAt.Format("2006-01-02"), dayAt(1).Format("2006-01-02"); got != want {
		t.Fatalf("earliestAt=%s want %s", got, want)
	}
	if earliestRem != 27 {
		t.Fatalf("earliestRemaining=%d want 27", earliestRem)
	}
	// 7 天内：11-11 的 27 + 11-12..11-17 各 30 = 207。
	if expiring != 207 {
		t.Fatalf("expiring=%d want 207（旧代码这里会是整池 %d）", expiring, int64(sumCap))
	}
}

// 单周期码保留历史行为：明细自报 0 时借用 summary 的当前周期剩余（只有一个周期，
// 不存在跨周期串味的风险）。这是 issue #200 第 2 条明确允许的兜底。
func TestGlobalExpirySingleCycleBorrowsSummary(t *testing.T) {
	rows := []expiryRow{
		{code: "PKG-SUB", end: dayAt(10), remain: 0, deduct: true, total: 500, summary: f64(45)},
	}
	_, _, _, earliestAt, earliestRem, buckets := snapshotWithRows(t, rows, 0)
	if len(buckets) != 1 || buckets[0].Remain != 45 {
		t.Fatalf("单周期码应借 summary 的 45，实际 %+v", buckets)
	}
	if earliestRem != 45 || earliestAt.IsZero() {
		t.Fatalf("earliest=%v/%d want 非零/45", earliestAt, earliestRem)
	}
}

// 多周期码**不借** summary：否则又回到「新周期余额挂到每个周期上」。
func TestGlobalExpiryMultiCycleNeverBorrowsSummary(t *testing.T) {
	rows := []expiryRow{
		{code: "PKG-BONUS", end: dayAt(5), remain: 0, deduct: true, total: 30},
		{code: "PKG-BONUS", end: dayAt(6), remain: 30, deduct: true, total: 30},
	}
	_, _, _, earliestAt, _, buckets := snapshotWithRows(t, rows, 0)
	if len(buckets) != 1 || buckets[0].Remain != 30 {
		t.Fatalf("多周期码不得借 summary，实际 %+v", buckets)
	}
	if got, want := earliestAt.Format("2006-01-02"), dayAt(6).Format("2006-01-02"); got != want {
		t.Fatalf("earliestAt=%s want %s", got, want)
	}
}

// 只有 CycleEndTime（缺 DeductionEndTime / ExpiredTime）时仍能算出到期日，
// 并标记为「按计量周期推算」——供调用方打 WARN、面板标注。
func TestPackageEndAtExMarksCycleFallback(t *testing.T) {
	end := dayAt(3)
	cycle := end.Format("2006-01-02 15:04:05")

	got, fromCycle, ok := packageEndAtEx(end.UnixMilli(), "", cycle)
	if !ok || fromCycle {
		t.Fatalf("有 DeductionEndTime 时不应标成推算：ok=%v fromCycle=%v", ok, fromCycle)
	}
	if !got.Equal(end) {
		t.Fatalf("got %v want %v", got, end)
	}

	got, fromCycle, ok = packageEndAtEx(0, cycle, "")
	if !ok || fromCycle {
		t.Fatalf("ExpiredTime 是权威字段，不应标成推算：ok=%v fromCycle=%v", ok, fromCycle)
	}
	if !got.Equal(end) {
		t.Fatalf("got %v want %v", got, end)
	}

	got, fromCycle, ok = packageEndAtEx(0, "", cycle)
	if !ok || !fromCycle {
		t.Fatalf("只剩 CycleEndTime 时应标成推算：ok=%v fromCycle=%v", ok, fromCycle)
	}
	if !got.Equal(end) {
		t.Fatalf("got %v want %v", got, end)
	}

	if _, fromCycle, ok := packageEndAtEx(0, "", ""); ok || fromCycle {
		t.Fatalf("三个字段全缺时应无到期：ok=%v fromCycle=%v", ok, fromCycle)
	}
}

// 明细里同一批同时出现在 paid / free 两条端点时只计一次（取剩余更大的那条）。
func TestGlobalExpiryDedupesSameCycleAcrossEndpoints(t *testing.T) {
	// 用同一个 end 的两条同码行模拟两条端点重复下发。
	rows := []expiryRow{
		{code: "PKG-BONUS", end: dayAt(4), remain: 12, deduct: true, total: 30},
		{code: "PKG-BONUS", end: dayAt(4), remain: 30, deduct: true, total: 30},
	}
	_, _, _, _, _, buckets := snapshotWithRows(t, rows, 0)
	if len(buckets) != 1 {
		t.Fatalf("同一周期重复下发只该有 1 个桶，实际 %+v", buckets)
	}
	if buckets[0].Remain != 30 {
		t.Fatalf("bucket remain=%d want 30（取剩余更大的那条）", buckets[0].Remain)
	}
}

var _ = json.Marshal

// 推算出来的到期日必须在桶上留标记，面板才能标注「按计量周期推算」（issue #200 第 4 条）。
func TestGlobalExpiryMarksEstimatedBucket(t *testing.T) {
	rows := []expiryRow{
		// 只有 CycleEndTime（缺 DeductionEndTime / ExpiredTime）→ 日期是推算的。
		{code: "PKG-OLD", end: dayAt(3), remain: 20, deduct: false, total: 30},
		// 有真实扣费截止 → 不标注。
		{code: "PKG-NEW", end: dayAt(9), remain: 40, deduct: true, total: 50},
	}
	_, _, _, _, _, buckets := snapshotWithRows(t, rows, 0)
	if len(buckets) != 2 {
		t.Fatalf("应有 2 个桶，实际 %+v", buckets)
	}
	if !buckets[0].Estimated {
		t.Fatalf("只靠 CycleEndTime 推算的那批必须标记 estimated：%+v", buckets[0])
	}
	if buckets[1].Estimated {
		t.Fatalf("有 DeductionEndTime 的批次不该标注：%+v", buckets[1])
	}
}
