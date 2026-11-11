package panel

import (
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 补领清扫侧的批量接受补偿用例。
//
// 复用 autotask_accept_fallback_test.go 的假上游与计数器（它已支持「批量失败 /
// 单码成功 / 按端标记分流 / 报错但已登记」四种形态）。sweepAccount 只认
// p.cfg.Upstream 这一个上游，假上游就是它的全部出口，因此这里断言的是真实请求序列。
//
// ctx 取消一项在清扫侧不适用：RunTaskSweepNow 由 scheduler 周期调用，没有 ctx
// （取消语义 = 关进程），sweepAccount 固定传 context.Background()，补偿循环靠
// 20 个上限收敛。取消即停由一键完成侧 TestAcceptBatchFallbackStopsOnContextCancel 覆盖。

// withZeroReportGap 补偿循环按 reportGap 节流；测试里置零，避免真实等待。
func withZeroReportGap(t *testing.T) {
	t.Helper()
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })
}

// sweepAcceptTarget 清扫调用面：同一个假上游、同一把账号。
func sweepAcceptTarget(t *testing.T, f *acceptFallbackFake) (*Panel, *auth.Auth) {
	t.Helper()
	return newAcceptFallbackPanel(t, f)
}

// codeListEqual 顺序敏感比较（假上游按请求顺序记账）。
func codeListEqual(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// 正常路径（批量一次成功）：每口径恰好一次批量 accept、零逐码请求，列表请求也不多打。
func TestSweepAcceptBatchSuccessSendsNoExtraRequest(t *testing.T) {
	withZeroReportGap(t)
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2"}, []string{"mp_1", "mp_2"})
	p, a := sweepAcceptTarget(t, f)

	accepted, claimed, err := p.sweepAccount(a)
	if err != nil {
		t.Fatalf("sweepAccount 出错: %v", err)
	}
	if accepted != 4 {
		t.Fatalf("接受数=%d want 4（默认 2 + mp 2）", accepted)
	}
	if claimed != 0 {
		t.Fatalf("领奖数=%d want 0（假任务未达标）", claimed)
	}
	singles, batches, mpBatches := f.snapshot()
	if batches != 1 || mpBatches != 1 {
		t.Fatalf("批量 accept 次数 default=%d mp=%d want 1/1", batches, mpBatches)
	}
	if len(singles) != 0 {
		t.Fatalf("批量成功时不应逐个重试，实际 %v", singles)
	}
	def, mp := f.listCounts()
	if def != 1 || mp != 1 {
		t.Fatalf("正常路径列表请求 default=%d mp=%d want 1/1（不得多打任何请求）", def, mp)
	}
}

// 默认口径批量失败：回读默认列表后逐码补偿默认口径，mp 口径不受影响。
func TestSweepAcceptBatchFallbackDefaultRealm(t *testing.T) {
	withZeroReportGap(t)
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2"}, nil)
	f.batchFail = true
	p, a := sweepAcceptTarget(t, f)

	accepted, _, err := p.sweepAccount(a)
	if err != nil {
		t.Fatalf("sweepAccount 出错: %v", err)
	}
	if accepted != 2 {
		t.Fatalf("逐码补偿成功后接受数=%d want 2", accepted)
	}
	def, mp := f.singleRealms()
	if !codeListEqual(def, "fb_1", "fb_2") {
		t.Fatalf("默认口径单码重试=%v want [fb_1 fb_2]", def)
	}
	if len(mp) != 0 {
		t.Fatalf("mp 口径不应有单码重试，实际 %v", mp)
	}
	_, batches, mpBatches := f.snapshot()
	if batches != 1 || mpBatches != 0 {
		t.Fatalf("批量 accept 次数 default=%d mp=%d want 1/0", batches, mpBatches)
	}
	dl, ml := f.listCounts()
	if dl != 2 || ml != 1 {
		t.Fatalf("批量失败后只应回读默认列表一次：default=%d mp=%d want 2/1", dl, ml)
	}
}

// mp 口径批量失败：回读 mp 列表后逐码补偿 mp 口径（缺 mp 头 accept 会 task not found）。
func TestSweepAcceptBatchFallbackMPRealm(t *testing.T) {
	withZeroReportGap(t)
	f := newAcceptFallbackFake(nil, []string{"mp_1", "mp_2"})
	f.batchFail = true
	p, a := sweepAcceptTarget(t, f)

	accepted, _, err := p.sweepAccount(a)
	if err != nil {
		t.Fatalf("sweepAccount 出错: %v", err)
	}
	if accepted != 2 {
		t.Fatalf("mp 逐码补偿成功后接受数=%d want 2", accepted)
	}
	def, mp := f.singleRealms()
	if len(def) != 0 {
		t.Fatalf("默认口径不应有单码重试，实际 %v", def)
	}
	if !codeListEqual(mp, "mp_1", "mp_2") {
		t.Fatalf("mp 口径单码重试=%v want [mp_1 mp_2]", mp)
	}
	_, batches, mpBatches := f.snapshot()
	if batches != 0 || mpBatches != 1 {
		t.Fatalf("批量 accept 次数 default=%d mp=%d want 0/1", batches, mpBatches)
	}
	dl, ml := f.listCounts()
	if dl != 1 || ml != 2 {
		t.Fatalf("批量失败后只应回读 mp 列表一次：default=%d mp=%d want 1/2", dl, ml)
	}
}
