// tasklive.go 面板「实时任务」：一次性读回所有账号当前的成长任务进度。
//
// 与任务中心的既有两种数据源的区别：
//   - tasks/jobs：后台任务**执行结果**（要有作业跑过才有内容）；
//   - tasks/scan_all + tasks/queue：手动触发的一次性扫描，跑完才有列表。
//
// 两者都要「有人先动一下」，看板上永远滞后。本文件提供**只读的实时视图**：
// 直接向每个账号查一次任务表，按 TTL 缓存后交给前端定时轮询，不需要任何手动动作。
//
// 为什么只调小程序口径一次：`ListTasksMP` 是默认口径的超集（上游实测），
// 全量 28 个号也只要 28 次请求；两个口径都调会翻倍，而结果差集为空。
// 国际版账号同样有此口径（实测返回 5 条）。
//
// 缓存的意义：浏览器每轮轮询都打一次上游，多开页面/多人看会成倍放大。
// TTL 内直接复用，`fresh=1` 可强制穿透（手动刷新按钮用）。
package panel

import (
	"net/http"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// taskLiveTTL 实时任务缓存有效期。取值考虑两点：上游任务进度是分钟级聚合的，
// 再密也不会更新；而 28 个号一轮就是 28 次上游 GET，压得太短会把看板变成压测。
const taskLiveTTL = 45 * time.Second

// taskLiveConcurrency 一轮实时查询的账号并发（与券码/活动页同档，避免打爆上游）。
const taskLiveConcurrency = 4

// liveTaskAccount 单账号的实时任务行。
type liveTaskAccount struct {
	UID      string          `json:"uid"`
	Nickname string          `json:"nickname"`
	Realm    string          `json:"realm"`
	Tasks    []upstream.Task `json:"tasks,omitempty"`
	// Pending  未完成的任务数（前端排序用）
	Pending int `json:"pending"`
	// Claimable 可领取的任务数
	Claimable int `json:"claimable"`
	// Claimed 已领取的任务数
	Claimed int    `json:"claimed"`
	Err     string `json:"error,omitempty"`
}

// liveCache 实时任务快照缓存（进程内，单面板实例）。
type liveCache struct {
	mu   sync.Mutex
	at   time.Time
	rows []liveTaskAccount
	// inflight 同一时刻只允许一轮上游查询：多标签页同时轮询时，
	// 后来的请求等前一**次**结果，而不是各自再打一遍 28 个号。
	inflight chan struct{}
}

var taskLive = &liveCache{}

// tasksLive GET /panel/api/tasks/live?fresh=1
//
// 返回所有启用账号的当前任务表。账号级失败只标在对应行（error 字段），
// 不影响其它账号——一个号的 token 过期不该让整个看板空掉。
func (p *Panel) tasksLive(w http.ResponseWriter, r *http.Request) {
	fresh := r.URL.Query().Get("fresh") == "1"

	taskLive.mu.Lock()
	if !fresh && len(taskLive.rows) > 0 && time.Since(taskLive.at) < taskLiveTTL {
		rows, at := taskLive.rows, taskLive.at
		taskLive.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "cached": true, "age_sec": int(time.Since(at).Seconds()),
			"ttl_sec": int(taskLiveTTL.Seconds()), "accounts": rows,
		})
		return
	}
	if taskLive.inflight != nil {
		// 已有一轮在跑：等它，别叠加。
		ch := taskLive.inflight
		taskLive.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			writeErr(w, http.StatusServiceUnavailable, "上一轮任务查询仍在进行，稍后再试")
			return
		}
		taskLive.mu.Lock()
		rows, at := taskLive.rows, taskLive.at
		taskLive.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "cached": true, "age_sec": int(time.Since(at).Seconds()),
			"ttl_sec": int(taskLiveTTL.Seconds()), "accounts": rows,
		})
		return
	}
	ch := make(chan struct{})
	taskLive.inflight = ch
	taskLive.mu.Unlock()

	rows := p.collectLiveTasks()

	taskLive.mu.Lock()
	taskLive.rows = rows
	taskLive.at = time.Now()
	taskLive.inflight = nil
	taskLive.mu.Unlock()
	close(ch)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "cached": false, "age_sec": 0,
		"ttl_sec": int(taskLiveTTL.Seconds()), "accounts": rows,
	})
}

// collectLiveTasks 并发查所有启用账号的任务表。
func (p *Panel) collectLiveTasks() []liveTaskAccount {
	stats := p.cfg.Pool.List()
	out := make([]liveTaskAccount, len(stats))
	sem := make(chan struct{}, taskLiveConcurrency)
	var wg sync.WaitGroup

	for i, st := range stats {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = liveTasksOf(p.cfg.Upstream, a)
		}(i, a)
	}
	wg.Wait()

	res := make([]liveTaskAccount, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	return res
}

// liveTasksOf 读一个账号的任务表并统计三类计数。
//
// 国内号只发小程序口径一次；国际号只发默认口径。失败不重试、不换口径。
func liveTasksOf(c *upstream.Client, a *auth.Auth) liveTaskAccount {
	it := liveTaskAccount{UID: a.UID, Nickname: a.Nickname, Realm: a.Realm()}
	if !a.SupportsPersonalEndpoints() {
		it.Err = noPersonalTaskMessage
		return it
	}
	// 国际号只走默认口径：小程序头对国际账号没有验证过的契约，单号任务弹窗
	// （accountTasks）同样明确不探 MP。
	// 每个账号每轮恰好一次上游请求：失败不重试、不换口径。重试会在上游抖动或一批号
	// 失效（封号潮）时把一轮的请求数翻倍，失败号等下一轮即可。
	var tasks []upstream.Task
	var err error
	if a.IsGlobal() {
		tasks, err = c.ListTasks(a)
	} else {
		tasks, err = c.ListTasksMP(a)
	}
	if err != nil {
		it.Err = err.Error()
		return it
	}
	it.Tasks = tasks
	for _, t := range tasks {
		switch {
		case t.Claimed:
			it.Claimed++
		case t.Claimable:
			it.Claimable++
		default:
			it.Pending++
		}
	}
	return it
}
