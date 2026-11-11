package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// acceptFallbackFake 假上游：任务列表按端标记下发 pending 码；批量 accept（>1 码）
// 可配置为失败，单码 accept 可配置为失败；记录每个单码调用，供断言补偿次数。
type acceptFallbackFake struct {
	mu         sync.Mutex
	tasks      []string
	mpTasks    []string
	batchFail  bool
	singleFail bool
	// batchFailButApply：批量回 500 但服务端其实已登记（上游存在「报错却生效」形态）。
	// 用来验证补偿只在回读仍为 not_accepted 时才动手，不做无谓的重复 accept。
	batchFailButApply bool
	cancel            context.CancelFunc
	accepted          map[string]bool
	singles           []string
	mpSingles         []string
	batches           int
	mpBatches         int
	lists             int
	mpLists           int
}

func newAcceptFallbackFake(tasks, mpTasks []string) *acceptFallbackFake {
	return &acceptFallbackFake{tasks: tasks, mpTasks: mpTasks, accepted: map[string]bool{}}
}

func (f *acceptFallbackFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mp := r.Header.Get("X-Client-Platform") != ""
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			f.mu.Lock()
			if mp {
				f.mpLists++
			} else {
				f.lists++
			}
			codes := f.tasks
			if mp {
				codes = f.mpTasks
			}
			accepted := make(map[string]bool, len(f.accepted))
			for k, v := range f.accepted {
				accepted[k] = v
			}
			f.mu.Unlock()
			tasks := make([]map[string]any, 0, len(codes))
			for _, c := range codes {
				status := "not_accepted"
				if accepted[c] {
					status = "accepted"
				}
				tasks = append(tasks, map[string]any{
					"task_code": c, "current": 0, "target": 1, "accept_status": status,
				})
			}
			writeAcceptFallbackJSON(w, map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": tasks}})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			batch := len(body.TaskCodes) > 1
			f.mu.Lock()
			if batch {
				if mp {
					f.mpBatches++
				} else {
					f.batches++
				}
			}
			fail := (f.batchFail && batch) || (f.singleFail && !batch)
			cancel := f.cancel
			apply := f.batchFailButApply
			f.mu.Unlock()
			if fail && !apply {
				if cancel != nil {
					cancel()
				}
				http.Error(w, `{"code":500,"msg":"one task code rejected"}`, http.StatusInternalServerError)
				return
			}
			f.mu.Lock()
			for _, c := range body.TaskCodes {
				f.accepted[c] = true
			}
			if !batch {
				f.singles = append(f.singles, body.TaskCodes...)
				if mp {
					f.mpSingles = append(f.mpSingles, body.TaskCodes...)
				}
			}
			f.mu.Unlock()
			if fail {
				http.Error(w, `{"code":500,"msg":"one task code rejected (已登记)"}`, http.StatusInternalServerError)
				return
			}
			writeAcceptFallbackJSON(w, map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		default:
			writeAcceptFallbackJSON(w, map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}
}

func writeAcceptFallbackJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *acceptFallbackFake) snapshot() (singles []string, batches, mpBatches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.singles...), f.batches, f.mpBatches
}

// listCounts 各口径的列表请求次数：正常路径每口径恰好 1 次（初始快照），
// 只有批量失败后的回读才会 +1——用来证明「正常路径没有多打任何请求」。
func (f *acceptFallbackFake) listCounts() (def, mp int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists, f.mpLists
}

// singleRealms 按口径分开的单码 accept 记录，供断言「mp 与默认口径各走对端点」。
func (f *acceptFallbackFake) singleRealms() (def, mp []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	def = append([]string(nil), f.singles...)
	for _, c := range f.mpSingles {
		def = removeOne(def, c)
	}
	return def, append([]string(nil), f.mpSingles...)
}

func removeOne(xs []string, v string) []string {
	for i, x := range xs {
		if x == v {
			return append(xs[:i:i], xs[i+1:]...)
		}
	}
	return xs
}

func newAcceptFallbackPanel(t *testing.T, f *acceptFallbackFake) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"}
	pl := pool.New("")
	pl.Add(a)
	return New(Config{Pool: pl, Upstream: up}), a
}

func findAcceptFallbackRow(res []map[string]any, code string) map[string]any {
	for _, r := range res {
		if r["task_code"] == code {
			return r
		}
	}
	return nil
}

func acceptFallbackMessage(row map[string]any) string {
	if row == nil {
		return ""
	}
	s, _ := row["message"].(string)
	return s
}

// TestAcceptBatchFallbackRetriesEachPendingCode 批量 accept 失败后回读仍有 3 个
// not_accepted 的码：逐个重试 3 次，并在 out 里输出补偿结果。
func TestAcceptBatchFallbackRetriesEachPendingCode(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := newAcceptFallbackFake([]string{"fb_1", "fb_2", "fb_3"}, nil)
	f.batchFail = true
	p, a := newAcceptFallbackPanel(t, f)
	res := p.runAutoAllObserved(context.Background(), a, nil)

	singles, batches, _ := f.snapshot()
	if batches != 1 {
		t.Fatalf("批量 accept 应只调用 1 次，实际 %d", batches)
	}
	if len(singles) != 3 {
		t.Fatalf("应逐个重试 3 个码，实际 %v", singles)
	}
	row := findAcceptFallbackRow(res, "(批量接受-补偿)")
	if row == nil {
		t.Fatalf("缺少补偿结果行，实际结果 %+v", res)
	}
	if row["status"] != "accepted" || !strings.Contains(acceptFallbackMessage(row), "成功 3/3") {
		t.Fatalf("补偿结果不符: %+v", row)
	}
}

// TestAcceptBatchSuccessMakesNoSingleCalls 正常路径（批量一次成功）不得产生任何
// 逐码重试请求，也不得多出补偿结果行。
func TestAcceptBatchSuccessMakesNoSingleCalls(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := newAcceptFallbackFake([]string{"fb_1", "fb_2", "fb_3"}, nil)
	p, a := newAcceptFallbackPanel(t, f)
	res := p.runAutoAllObserved(context.Background(), a, nil)

	singles, batches, _ := f.snapshot()
	if batches != 1 {
		t.Fatalf("批量 accept 应调用 1 次，实际 %d", batches)
	}
	if len(singles) != 0 {
		t.Fatalf("批量成功时不应逐个重试，实际 %v", singles)
	}
	if row := findAcceptFallbackRow(res, "(批量接受-补偿)"); row != nil {
		t.Fatalf("批量成功时不应输出补偿行: %+v", row)
	}
	row := findAcceptFallbackRow(res, "(批量接受)")
	if row == nil || row["status"] != "accepted" {
		t.Fatalf("批量成功结果行不符: %+v", row)
	}
}

// TestAcceptBatchFallbackStopsAtLimit 待补偿码超过上限时只重试上限个并停止，
// 结果里说明剩余数量。
func TestAcceptBatchFallbackStopsAtLimit(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	codes := make([]string, acceptBatchFallbackLimit+5)
	for i := range codes {
		codes[i] = fmt.Sprintf("fb_%02d", i)
	}
	f := newAcceptFallbackFake(codes, nil)
	f.batchFail = true
	p, a := newAcceptFallbackPanel(t, f)
	res := p.runAutoAllObserved(context.Background(), a, nil)

	singles, _, _ := f.snapshot()
	if len(singles) != acceptBatchFallbackLimit {
		t.Fatalf("应只重试上限 %d 个码，实际 %d", acceptBatchFallbackLimit, len(singles))
	}
	row := findAcceptFallbackRow(res, "(批量接受-补偿)")
	if row == nil {
		t.Fatalf("缺少补偿结果行，实际结果 %+v", res)
	}
	msg := acceptFallbackMessage(row)
	if !strings.Contains(msg, "上限 20") || !strings.Contains(msg, "剩余 5") {
		t.Fatalf("补偿结果应说明上限与剩余数量，实际 %q", msg)
	}
}

// TestAcceptBatchFallbackStopsOnContextCancel ctx 取消后逐个重试必须立即停止。
func TestAcceptBatchFallbackStopsOnContextCancel(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := newAcceptFallbackFake([]string{"fb_1", "fb_2", "fb_3"}, nil)
	f.batchFail = true
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	t.Cleanup(cancel)
	p, a := newAcceptFallbackPanel(t, f)
	res := p.runAutoAllObserved(ctx, a, nil)

	singles, batches, _ := f.snapshot()
	if batches != 1 {
		t.Fatalf("批量 accept 应只调用 1 次，实际 %d", batches)
	}
	if len(singles) != 0 {
		t.Fatalf("ctx 取消后不应再逐个重试，实际 %v", singles)
	}
	row := findAcceptFallbackRow(res, "(批量接受-补偿)")
	if row == nil || !strings.Contains(acceptFallbackMessage(row), "已取消") {
		t.Fatalf("补偿结果应说明已取消: %+v", row)
	}
}

// TestAcceptBatchFallbackMPRetriesEachCode mp 口径批量失败后同样逐码补偿。
func TestAcceptBatchFallbackMPRetriesEachCode(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := newAcceptFallbackFake(nil, []string{"mp_1", "mp_2"})
	f.batchFail = true
	p, a := newAcceptFallbackPanel(t, f)
	res := p.runAutoAllObserved(context.Background(), a, nil)

	singles, batches, mpBatches := f.snapshot()
	if batches != 0 || mpBatches != 1 {
		t.Fatalf("默认口径不应有批量调用（%d），mp 批量应 1 次（%d）", batches, mpBatches)
	}
	if len(singles) != 2 {
		t.Fatalf("mp 应逐个重试 2 个码，实际 %v", singles)
	}
	row := findAcceptFallbackRow(res, "(批量接受-mp-补偿)")
	if row == nil || !strings.Contains(acceptFallbackMessage(row), "成功 2/2") {
		t.Fatalf("mp 补偿结果不符: %+v", row)
	}
}

// TestAcceptBatchFallbackSkipsRetryWhenReadbackAlreadyAccepted 批量回 500 但服务端
// 其实已经登记（上游存在「报错却生效」形态）：判据是回读 accept_status，不是 200/500。
// 回读已全部生效时必须零逐码请求，避免补偿本身变成重复 accept。
func TestAcceptBatchFallbackSkipsRetryWhenReadbackAlreadyAccepted(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := newAcceptFallbackFake([]string{"fb_1", "fb_2"}, nil)
	f.batchFail = true
	f.batchFailButApply = true
	p, a := newAcceptFallbackPanel(t, f)
	res := p.runAutoAllObserved(context.Background(), a, nil)

	singles, batches, _ := f.snapshot()
	if batches != 1 {
		t.Fatalf("批量 accept 应只调用 1 次，实际 %d", batches)
	}
	if len(singles) != 0 {
		t.Fatalf("回读确认已全部生效时不应逐个重试，实际 %v", singles)
	}
	row := findAcceptFallbackRow(res, "(批量接受-补偿)")
	if row == nil || row["status"] != "accepted" {
		t.Fatalf("回读确认生效后状态应为 accepted: %+v", row)
	}
	if !strings.Contains(acceptFallbackMessage(row), "没有待接受的码") {
		t.Fatalf("补偿说明应指出回读无待接受: %q", acceptFallbackMessage(row))
	}
}
