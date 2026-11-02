package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// missingCodeFake 假上游：任务列表只下发 codes 里的 code，其余请求一律 200 空信封。
// 记录所有请求的 path+body，供断言「没有为不存在的任务打上游」。
type missingCodeFake struct {
	codes  []string
	accept string // 列表响应下发的 accept_status；空 = 不带该字段（进度未知）
	mu     sync.Mutex
	seen   []string
	mpErr  bool // true = mp 口径任务列表回 500（列表拿不到 → 不筛选）
}

func (f *missingCodeFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var buf strings.Builder
		buf.WriteString(r.URL.Path)
		if r.Header.Get("X-Client-Platform") != "" {
			buf.WriteString(" mp")
		}
		if b, _ := readAllBody(r); len(b) > 0 {
			buf.WriteString(" ")
			buf.Write(b)
		}
		f.mu.Lock()
		f.seen = append(f.seen, buf.String())
		mp, mpErr := r.Header.Get("X-Client-Platform") != "", f.mpErr
		f.mu.Unlock()

		if strings.Contains(r.URL.Path, "/growth/tasks") {
			if mp && mpErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			tasks := make([]any, 0, len(f.codes))
			for _, c := range f.codes {
				t := map[string]any{"task_code": c, "current": 0, "target": 1}
				if f.accept != "" {
					t["accept_status"] = f.accept
					t["current"], t["target"] = 1, 1
				}
				tasks = append(tasks, t)
			}
			body, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": tasks}})
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}
}

func readAllBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

// missingCodePanel 装配指向 fake 上游的面板。接受 *missingCodeFake 或其扩展
// （如 claimedFake，经 asMissing 接口解引用回内嵌基座）。
type fakeProvider interface{ handler() http.HandlerFunc }

func missingCodePanel(t *testing.T, f fakeProvider) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at"}
	pl := pool.New("")
	pl.Add(a)
	return New(Config{Pool: pl, Upstream: up}), a
}

// TestRunAutoAllSkipsCodesAccountDoesNotHave 账号任务列表里没有的 code 不再打上游
// （线上 26 个 CN 号每轮为 4 个已下线的任务白打 104 次调用），但结果行照旧是 skipped。
func TestRunAutoAllSkipsCodesAccountDoesNotHave(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := &missingCodeFake{codes: []string{"chat_5"}}
	p, a := missingCodePanel(t, f)
	res := p.runAutoAllObserved(nil, a, nil)

	var sawSkipped bool
	for _, r := range res {
		if r["task_code"] == "template_5" {
			sawSkipped = r["status"] == "skipped" && r["message"] == "该账号没有此任务"
		}
	}
	if !sawSkipped {
		t.Errorf("template_5 应输出 skipped 行，实际结果 %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.seen {
		if strings.Contains(req, "template_5") {
			t.Fatalf("不该为账号没有的任务打上游: %s", req)
		}
	}
}

// TestRunAutoAllNoFilterWhenListFails 列表读失败时不能筛选：一次读失败不该把所有任务跳过。
func TestRunAutoAllNoFilterWhenListFails(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := &missingCodeFake{codes: []string{"chat_5"}, mpErr: true}
	p, a := missingCodePanel(t, f)
	res := p.runAutoAllObserved(nil, a, nil)
	if len(res) == 0 {
		t.Fatal("列表失败时仍应逐项执行并输出结果")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var sawNonList bool
	for _, req := range f.seen {
		if !strings.Contains(req, "/growth/tasks") {
			sawNonList = true
		}
	}
	if !sawNonList {
		t.Error("列表失败时应照旧执行动作，不能只顾跳过")
	}
}

// TestRunAutoAllSnapshotSkipsClaimed 阶段 0 快照已确认「已领奖」的任务不再逐项重拉
// 列表（issue #149）：整轮列表查询次数应停在阶段 0 的两次（默认 + mp），而不是
// 每个已领任务各多打一次。fake 的列表响应把所有 code 都标成 claimed。
func TestRunAutoAllSnapshotSkipsClaimed(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f := &claimedFake{missingCodeFake: missingCodeFake{codes: []string{"chat_5", "template_5", "Sequential_Tasks_1"}, accept: "claimed"}}
	p, a := missingCodePanel(t, f)
	res := p.runAutoAllObserved(nil, a, nil)

	var skipped int
	for _, r := range res {
		if r["status"] == "skipped" {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatalf("已领奖任务应输出 skipped 行: %+v", res)
	}
	if got := f.listCalls(); got != 2 {
		t.Fatalf("列表查询 %d 次，want 2（阶段 0 默认+mp 各一次，逐项不再拉）", got)
	}
}

// TestRunAutoAllUniformGapBetweenItems 每项之间统一留 reportGap：原来只在「执行了
// 动作」后等，查询类（进度未满、这一项没动作可跑）的项零间隔背靠背打上游，是
// issue #149 里 429 的来源。快照跳过项不打上游，不占 gap。
func TestRunAutoAllUniformGapBetweenItems(t *testing.T) {
	old := reportGap
	reportGap = 150 * time.Millisecond
	t.Cleanup(func() { reportGap = old })

	// 三个任务进度都已达标但没领奖：逐项都会查上游 + 领奖，但跑不出动作
	// （ActionRun=false），正好落在旧逻辑「只在 ActionRun 后等」的空档里。
	f := &missingCodeFake{codes: []string{"chat_5", "template_5", "Buddy_App"}, accept: "accepted"}
	p, a := missingCodePanel(t, f)
	start := time.Now()
	_ = p.runAutoAllObserved(nil, a, nil)
	// 旧逻辑（只在 ActionRun 后等）这三项之间零间隔，整轮远小于 3×gap。
	if elapsed := time.Since(start); elapsed < 3*reportGap {
		t.Fatalf("整轮耗时 %s < 3×gap，查询类项之间没有统一节流", elapsed.Round(time.Millisecond))
	}
}

// claimedFake 列表把全部任务下发为 claimed（已领奖）的 fake，并数列表查询次数。
type claimedFake struct {
	missingCodeFake
	mu2   sync.Mutex
	lists int
}

// Handler 覆写：只统计列表查询次数，列表内容由 missingCodeFake.accept 决定。
func (f *claimedFake) handler() http.HandlerFunc {
	inner := f.missingCodeFake.handler()
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/growth/tasks") {
			f.mu2.Lock()
			f.lists++
			f.mu2.Unlock()
		}
		inner(w, r)
	}
}

func (f *claimedFake) listCalls() int {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	return f.lists
}
