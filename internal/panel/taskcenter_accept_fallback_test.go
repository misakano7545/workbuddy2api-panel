package panel

import "testing"

// 成长队列前置 acceptPendingTasks（taskcenter.go:520，调用点 taskcenter.go:454）的
// 批量接受补偿用例。复用 autotask_accept_fallback_test.go 的假上游与计数器。

// 批量失败后逐个补偿成功：返回值等于补偿成功的个数（调用点 >0 才会等 reportGap）。
func TestAcceptPendingTasksFallsBackToSingleCodes(t *testing.T) {
	withZeroReportGap(t)
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2", "fb_3"}, nil)
	f.batchFail = true
	p, a := newAcceptFallbackPanel(t, f)

	if got := p.acceptPendingTasks(a); got != 3 {
		t.Fatalf("acceptPendingTasks=%d want 3（批量失败后逐个补偿成功的个数）", got)
	}
	def, mp := f.singleRealms()
	if !codeListEqual(def, "fb_1", "fb_2", "fb_3") {
		t.Fatalf("默认口径单码请求=%v want [fb_1 fb_2 fb_3]", def)
	}
	if len(mp) != 0 {
		t.Fatalf("队列路径只走默认口径，实际 mp 单码请求 %v", mp)
	}
	_, batches, mpBatches := f.snapshot()
	if batches != 1 || mpBatches != 0 {
		t.Fatalf("批量 accept default=%d mp=%d want 1/0", batches, mpBatches)
	}
	dl, ml := f.listCounts()
	if dl != 2 || ml != 0 {
		t.Fatalf("列表请求 default=%d mp=%d want 2/0（初始 + 补偿回读）", dl, ml)
	}
}

// 正常路径（批量一次成功）：零单码请求、零额外列表请求。
func TestAcceptPendingTasksSuccessSendsNoExtraRequest(t *testing.T) {
	withZeroReportGap(t)
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2"}, nil)
	p, a := newAcceptFallbackPanel(t, f)

	if got := p.acceptPendingTasks(a); got != 2 {
		t.Fatalf("acceptPendingTasks=%d want 2", got)
	}
	singles, batches, _ := f.snapshot()
	if len(singles) != 0 {
		t.Fatalf("批量成功时不得逐个重试，实际 %v", singles)
	}
	if batches != 1 {
		t.Fatalf("批量 accept=%d want 1", batches)
	}
	dl, ml := f.listCounts()
	if dl != 1 || ml != 0 {
		t.Fatalf("正常路径不得多打请求 default=%d mp=%d want 1/0", dl, ml)
	}
}

// 全部失败仍不阻塞：返回 0，调用方照常继续（「失败不阻塞」语义保留）。
func TestAcceptPendingTasksStillNonBlockingWhenAllFail(t *testing.T) {
	withZeroReportGap(t)
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2"}, nil)
	f.batchFail = true
	f.singleFail = true
	p, a := newAcceptFallbackPanel(t, f)

	if got := p.acceptPendingTasks(a); got != 0 {
		t.Fatalf("全失败时返回=%d want 0（保留「失败不阻塞」）", got)
	}
}
