package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 手动「全部接受」端点（taskAcceptAll）的批量接受补偿用例。
//
// 复用 autotask_accept_fallback_test.go 的假上游与计数器，以及
// tasksweep_accept_fallback_test.go 的 withZeroReportGap/codeListEqual。假上游是
// p.cfg.Upstream 的唯一出口，因此这里断言的是真实请求序列（次数 + 码），
// 而不是「函数被调过」。

// withZeroAcceptBatchGap 端点按 acceptBatchGap 逐片节流；测试置零，避免真实等待。
func withZeroAcceptBatchGap(t *testing.T) {
	t.Helper()
	old := acceptBatchGap
	acceptBatchGap = 0
	t.Cleanup(func() { acceptBatchGap = old })
}

// acceptAllResponse 走一遍 taskAcceptAll handler（含 PathValue），返回状态码、解出的
// 响应体与原始 JSON。
func acceptAllResponse(t *testing.T, p *Panel, a *auth.Auth) (int, map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/accounts/"+a.UID+"/tasks/accept-all", nil)
	r.SetPathValue("uid", a.UID)
	w := httptest.NewRecorder()
	p.taskAcceptAll(w, r)
	raw := w.Body.String()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("响应不是 JSON: %q (%v)", raw, err)
	}
	return w.Code, body, raw
}

// 分片批量失败但逐个补偿全部成功：单码请求数 = 该片码数，accepted 计入补偿成功的码，
// failed 为空。
func TestTaskAcceptAllSliceFailureFallsBackToSingleCodes(t *testing.T) {
	withZeroReportGap(t)
	withZeroAcceptBatchGap(t)
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2", "fb_3"}, nil)
	f.batchFail = true
	p, a := newAcceptFallbackPanel(t, f)

	code, body, raw := acceptAllResponse(t, p, a)
	if code != http.StatusOK {
		t.Fatalf("HTTP=%d body=%s", code, raw)
	}
	if body["accepted"] != float64(3) {
		t.Fatalf("accepted=%v want 3（补偿成功的码要计入）body=%s", body["accepted"], raw)
	}
	if body["failed"] != nil {
		t.Fatalf("failed=%v want null（逐个重试全部成功后不得再有失败码）body=%s", body["failed"], raw)
	}
	if _, has := body["message"]; has {
		t.Fatalf("全部补偿成功时不应带 message: %s", raw)
	}
	def, mp := f.singleRealms()
	if !codeListEqual(def, "fb_1", "fb_2", "fb_3") {
		t.Fatalf("默认口径单码请求=%v want [fb_1 fb_2 fb_3]", def)
	}
	if len(mp) != 0 {
		t.Fatalf("mp 口径不应有单码请求，实际 %v", mp)
	}
	// 批量 accept 恰好 2 次：第 1 次是本端点的失败探测，第 2 次是 acceptPendingWithFallback
	// 自己的首次提交（accept 幂等，等价于给这片一次重试），之后才是按码补偿的 3 次单码请求。
	singles, batches, mpBatches := f.snapshot()
	if len(singles) != 3 || batches != 2 || mpBatches != 0 {
		t.Fatalf("单码=%v 批量 default=%d mp=%d want 3 / 2 / 0", singles, batches, mpBatches)
	}
	dl, ml := f.listCounts()
	if dl != 2 || ml != 1 {
		t.Fatalf("列表请求 default=%d mp=%d want 2/1（初始 + helper 回读；失败归属回读应被跳过）", dl, ml)
	}
}

// mp 口径超过一片（25 个码）时必须分片提交，不能一次全量——一次全量正是本次要修的缺陷。
func TestTaskAcceptAllMPSlicesInsteadOfOneFullBatch(t *testing.T) {
	withZeroReportGap(t)
	withZeroAcceptBatchGap(t)
	mpCodes := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		mpCodes = append(mpCodes, fmt.Sprintf("mp_%02d", i))
	}
	// 默认口径给 2 个码：假上游按「请求里 >1 个码」才记批量，1 个码的提交与单码补偿
	// 无法区分，两个口径都必须是真正的分片提交才能用计数器断言。
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2"}, mpCodes)
	p, a := newAcceptFallbackPanel(t, f)

	code, body, raw := acceptAllResponse(t, p, a)
	if code != http.StatusOK {
		t.Fatalf("HTTP=%d body=%s", code, raw)
	}
	if body["accepted"] != float64(27) {
		t.Fatalf("accepted=%v want 27（默认 2 + mp 25）body=%s", body["accepted"], raw)
	}
	_, batches, mpBatches := f.snapshot()
	if batches != 1 {
		t.Fatalf("默认口径批量 accept=%d want 1", batches)
	}
	if mpBatches != 2 {
		t.Fatalf("mp 口径 25 个码应分 2 片（每片 20）提交，实际批量 accept=%d 次（=1 即回归「一次全量」）", mpBatches)
	}
	singles, _, _ := f.snapshot()
	if len(singles) != 0 {
		t.Fatalf("全部成功时不应有单码请求，实际 %v", singles)
	}
	dl, ml := f.listCounts()
	if dl != 1 || ml != 1 {
		t.Fatalf("列表请求 default=%d mp=%d want 1/1", dl, ml)
	}
}

// 正常路径（各片一次成功）：零单码请求、零额外列表请求，HTTP 响应字段与改动前逐字一致。
func TestTaskAcceptAllSuccessKeepsResponseAndSendsNoExtraRequest(t *testing.T) {
	withZeroReportGap(t)
	withZeroAcceptBatchGap(t)
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2"}, []string{"mp_1", "mp_2"})
	p, a := newAcceptFallbackPanel(t, f)

	code, body, raw := acceptAllResponse(t, p, a)
	if code != http.StatusOK {
		t.Fatalf("HTTP=%d body=%s", code, raw)
	}
	// 改动前：resp = {ok, accepted, failed(可能 null)}，失败才追加 message。
	if len(body) != 3 || body["ok"] != true || body["accepted"] != float64(4) || body["failed"] != nil {
		t.Fatalf("响应字段与改动前不一致（应为 {ok:true,accepted:4,failed:null}）: %s", raw)
	}
	if _, has := body["message"]; has {
		t.Fatalf("全部接受成功时不应带 message: %s", raw)
	}
	singles, batches, mpBatches := f.snapshot()
	if len(singles) != 0 {
		t.Fatalf("正常路径不得有任何单码请求，实际 %v", singles)
	}
	if batches != 1 || mpBatches != 1 {
		t.Fatalf("批量 accept default=%d mp=%d want 1/1", batches, mpBatches)
	}
	dl, ml := f.listCounts()
	if dl != 1 || ml != 1 {
		t.Fatalf("正常路径不得多打列表请求 default=%d mp=%d want 1/1", dl, ml)
	}
}

// 片内一个坏码：failed 必须精确到「回读确认仍未被接受」的那个码，不能把整片都记失败
// （改动前整片 20 个全进 failed，用户会去重试已经接受的码）。
func TestTaskAcceptAllFailedListNamesOnlyStillPendingCodes(t *testing.T) {
	withZeroReportGap(t)
	withZeroAcceptBatchGap(t)
	f := newAcceptFallbackFake([]string{"fb_1", "fb_2"}, nil)
	f.batchFail = true
	f.singleFailCodes = map[string]bool{"fb_2": true}
	p, a := newAcceptFallbackPanel(t, f)

	code, body, raw := acceptAllResponse(t, p, a)
	if code != http.StatusOK {
		t.Fatalf("HTTP=%d body=%s", code, raw)
	}
	if body["accepted"] != float64(1) {
		t.Fatalf("accepted=%v want 1（fb_1 补偿成功、fb_2 仍被拒）body=%s", body["accepted"], raw)
	}
	failed, _ := body["failed"].([]any)
	if len(failed) != 1 || failed[0] != "fb_2" {
		t.Fatalf("failed=%v want [fb_2]（不得把整片都记失败）body=%s", failed, raw)
	}
	if body["message"] != "部分任务接受失败（上游拒绝），可重试" {
		t.Fatalf("部分失败时 message 应与改动前一致，实际 %v", body["message"])
	}
	dl, _ := f.listCounts()
	if dl != 3 {
		t.Fatalf("默认口径列表请求=%d want 3（初始 + helper 回读 + 失败归属回读）", dl)
	}
}

// listErrorPanel 所有上游请求都 500 的面板：覆盖 acceptFailedCodes 回读失败/ctx 取消
// 的兜底分支（异常路径的异常路径，不能靠 httptest 假上游的服务端行为构造）。
func listErrorPanel(t *testing.T) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":500,"msg":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"}
	pl := pool.New("")
	pl.Add(a)
	return New(Config{Pool: pl, Upstream: up}), a
}

func TestAcceptFailedCodesFallsBackToCountWhenReadbackUnavailable(t *testing.T) {
	p, a := listErrorPanel(t)

	got := p.acceptFailedCodes(context.Background(), a, []string{"fb_1", "fb_2", "fb_3"}, 1, false)
	if !codeListEqual(got, "fb_2", "fb_3") {
		t.Fatalf("回读失败时应保守回报未确认的码 [fb_2 fb_3]，实际 %v", got)
	}
	if got := p.acceptFailedCodes(context.Background(), a, []string{"fb_1", "fb_2"}, 2, false); len(got) != 0 {
		t.Fatalf("全部确认接受时不得回报任何失败码，实际 %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got = p.acceptFailedCodes(ctx, a, []string{"fb_1", "fb_2"}, 0, false)
	if !codeListEqual(got, "fb_1", "fb_2") {
		t.Fatalf("ctx 取消、未做回读时应保守回报全部未确认码，实际 %v", got)
	}
}
