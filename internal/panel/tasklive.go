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
// 再密也不会更新；而一轮是「启用账号数」次上游 GET，一个打开的面板每秒的请求量 =
// 账号数 / 缓存时长，压得太短（原 45 秒）会把看板变成压测。
const taskLiveTTL = 60 * time.Second

// taskLiveConcurrency 一轮实时查询的账号并发（与券码/活动页同档，避免打爆上游）。
const taskLiveConcurrency = 4

// taskLiveFreshFloor fresh=1 的下限。变量只为让测试能压缩时间，生产不改。
var taskLiveFreshFloor = 15 * time.Second

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

// liveCache 实时任务快照（挂在 Panel 上，随面板实例走）。零值可用。
type liveCache struct {
	mu   sync.Mutex
	at   time.Time // 最近一轮完成的时刻；零值 = 还没跑完过
	rows []liveTaskAccount
	// running 非 nil 表示有一轮在后台跑，轮次结束时关闭。同一时刻只跑一轮：多标签页
	// 同时轮询时共用它，而不是各自再打一遍所有账号。
	running chan struct{}
}

// taskLiveWait 单个请求最多等一轮多久，比常见反代 60s 读超时短。变量只为让测试能压缩
// 时间，生产不改。
var taskLiveWait = 25 * time.Second

// taskLiveSnapshot 取实时任务快照，必要时在后台起一轮。
//
// 一轮在后台 goroutine 里跑、不绑请求：账号多、个别上游挂住（单次最长 120s）时，一轮
// 可以比反代超时和 30s 更久。请求最多等 taskLiveWait，之后带着上一份快照返回
// （refreshing=true），不会被反代截断，也不会让后来的请求排队等到 503。
//
// 返回 waited=true 表示本次请求等过一轮；refreshing=true 表示返回时仍有一轮在跑
// （返回的是上一份快照，从没跑完过则为空）。
func (p *Panel) taskLiveSnapshot(fresh bool) (rows []liveTaskAccount, at time.Time, waited, refreshing bool) {
	c := &p.taskLive
	c.mu.Lock()
	age := time.Since(c.at)
	stale := c.at.IsZero() || age >= taskLiveTTL || (fresh && age >= taskLiveFreshFloor)
	if stale && c.running == nil {
		done := make(chan struct{})
		c.running = done
		go func() {
			rows := p.collectLiveTasks()
			c.mu.Lock()
			c.rows, c.at, c.running = rows, time.Now(), nil
			c.mu.Unlock()
			close(done)
		}()
	}
	// 没有快照可给，或用户明确要新的：等这一轮（有上限）。其余情况直接回现有快照，
	// 过期的由后台那一轮顶上，下一次读取就是新的。
	done := c.running
	wait := done != nil && (c.at.IsZero() || fresh)
	c.mu.Unlock()

	if wait {
		waited = true
		select {
		case <-done:
		case <-time.After(taskLiveWait):
		}
	}
	c.mu.Lock()
	rows, at, refreshing = c.rows, c.at, c.running != nil
	c.mu.Unlock()
	return rows, at, waited, refreshing
}

// tasksLive GET /panel/api/tasks/live?fresh=1
//
// 返回所有启用账号的当前任务表。账号级失败只标在对应行（error 字段），
// 不影响其它账号——一个号的 token 过期不该让整个看板空掉。
func (p *Panel) tasksLive(w http.ResponseWriter, r *http.Request) {
	rows, at, waited, refreshing := p.taskLiveSnapshot(r.URL.Query().Get("fresh") == "1")
	age := 0
	if !at.IsZero() {
		age = int(time.Since(at).Seconds())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "cached": !waited, "age_sec": age, "refreshing": refreshing,
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
