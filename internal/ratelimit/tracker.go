package ratelimit

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Decision 是请求路径的限流判定（零值 = 放行）。
type Decision struct {
	Action string `json:"action,omitempty"` // degrade | hold | ""
	Target string `json:"target,omitempty"` // degrade 的目标模型
	Kind   string `json:"kind,omitempty"`   // tokens | pool_credit | accounts
	Reason string `json:"reason,omitempty"`
	Realm  string `json:"realm,omitempty"`
	Model  string `json:"model,omitempty"`
	// RetryAfterSec 是建议重试秒数（窗口内最早有额度滚出的时刻；0 = 不给建议）。
	RetryAfterSec int `json:"retry_after_sec,omitempty"`
}

// Allow 报告这次请求是否原样放行。
func (d Decision) Allow() bool { return d.Action == "" }

// TrackerConfig 装配一个 Tracker。
type TrackerConfig struct {
	// Path 是策略文件路径；空串表示只在内存里跑（测试/未配置持久化时）。
	Path string
	Deps Deps
	// RefreshEvery 是后台重算周期；<=0 用 30s。
	RefreshEvery time.Duration
	Logf         func(format string, args ...any)
}

const defaultRefreshEvery = 30 * time.Second

// chainRow 是某 (域, 模型) 的链缓存，供 6004 逐级降级用。
type chainRow struct {
	steps  []Step
	source string
}

// Tracker 是策略 + 快照 + 判定的持有者。
//
// 请求路径上每一次 Decide 都必须是零锁的：选号前已经在做几十件事，
// 不能在这里再抢一把可能被页面 GET（构建整张表，几十毫秒）占住的互斥锁。
// 所以快照与判定表都在后台按 RefreshEvery 预计算，只有写策略（低频）
// 才拿锁并立即重算一次。
type Tracker struct {
	cfg TrackerConfig

	mu      sync.Mutex
	policy  Policy
	lastErr error

	view      atomic.Pointer[View]
	decisions atomic.Pointer[map[string]Decision]
	chains    atomic.Pointer[map[string]chainRow]
}

// NewTracker 读取策略文件并算出首份快照。nil 安全：路径为空也能用。
func NewTracker(cfg TrackerConfig) *Tracker {
	if cfg.RefreshEvery <= 0 {
		cfg.RefreshEvery = defaultRefreshEvery
	}
	t := &Tracker{cfg: cfg}
	p, err := LoadPolicy(cfg.Path)
	if err != nil {
		t.lastErr = err
		t.logf("模型限流映射：%v", err)
	}
	t.policy = NormalizePolicy(p)
	t.Refresh()
	return t
}

func (t *Tracker) logf(format string, args ...any) {
	if t == nil || t.cfg.Logf == nil {
		return
	}
	t.cfg.Logf(format, args...)
}

// Start 起后台刷新循环。ctx 取消即退出（随进程关闭一起走）。
func (t *Tracker) Start(ctx context.Context) {
	if t == nil {
		return
	}
	go func() {
		tk := time.NewTicker(t.cfg.RefreshEvery)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				t.Refresh()
			}
		}
	}()
}

// Refresh 重算快照、判定表与链缓存。写策略后立即调用，让页面与请求路径同时生效。
func (t *Tracker) Refresh() {
	if t == nil {
		return
	}
	t.mu.Lock()
	p := clonePolicy(t.policy)
	t.mu.Unlock()

	v := Build(p, t.cfg.Deps)
	decisions := make(map[string]Decision, len(v.Rows))
	chains := make(map[string]chainRow, len(v.Rows))
	for _, row := range v.Rows {
		key := decideKey(row.Realm, row.Model)
		d := DecisionOf(row)
		d.Realm, d.Model = row.Realm, row.Model
		decisions[key] = d
		chains[key] = chainRow{steps: row.Chain, source: row.ChainSource}
	}
	t.view.Store(&v)
	t.decisions.Store(&decisions)
	t.chains.Store(&chains)
}

// View 返回最近一次快照（只读；调用方不得修改其中的切片）。
func (t *Tracker) View() View {
	if t == nil {
		return View{Presets: Presets(), Summary: map[string]RealmSummary{}}
	}
	if v := t.view.Load(); v != nil {
		return *v
	}
	return View{Presets: Presets(), Summary: map[string]RealmSummary{}}
}

// Policy 返回当前策略副本。
func (t *Tracker) Policy() Policy {
	if t == nil {
		return DefaultPolicy()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return clonePolicy(t.policy)
}

// Err 返回最近一次加载/落盘错误（页面顶部提示用），成功一次即清空。
func (t *Tracker) Err() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr
}

// Save 校验并落盘策略，成功后立即重算快照。
func (t *Tracker) Save(p Policy) (Policy, error) {
	if t == nil {
		return p, fmt.Errorf("模型限流映射未启用")
	}
	p = NormalizePolicy(p)
	p.UpdatedAt = time.Now().UTC()
	if err := Validate(p); err != nil {
		return t.Policy(), err
	}
	if err := SavePolicy(t.cfg.Path, p); err != nil {
		t.mu.Lock()
		t.lastErr = err
		t.mu.Unlock()
		return t.Policy(), err
	}
	t.mu.Lock()
	t.policy = p
	t.lastErr = nil
	t.mu.Unlock()
	t.Refresh()
	return clonePolicy(p), nil
}

// Upsert 新增/替换一条覆盖项（作用域为域+模型时；全空的域与模型表示改默认项）。
func (t *Tracker) Upsert(l ModelLimit) (Policy, error) {
	if t == nil {
		return l2p(l), fmt.Errorf("模型限流映射未启用")
	}
	n := Normalize(l)
	if err := ValidateLimit(n); err != nil {
		return t.Policy(), err
	}
	p := t.Policy()
	if n.Realm == "" && n.Model == "" {
		p.Default = n
		return t.Save(p)
	}
	replaced := false
	for i := range p.Overrides {
		if sameScope(p.Overrides[i], n) {
			p.Overrides[i] = n
			replaced = true
			break
		}
	}
	if !replaced {
		p.Overrides = append(p.Overrides, n)
	}
	return t.Save(p)
}

// Delete 删除一条覆盖项（按域+模型匹配，忽略大小写）。
func (t *Tracker) Delete(realm, model string) (Policy, error) {
	if t == nil {
		return DefaultPolicy(), fmt.Errorf("模型限流映射未启用")
	}
	want := Normalize(ModelLimit{Realm: realm, Model: model})
	p := t.Policy()
	kept := make([]ModelLimit, 0, len(p.Overrides))
	for _, o := range p.Overrides {
		if sameScope(o, want) {
			continue
		}
		kept = append(kept, o)
	}
	p.Overrides = kept
	return t.Save(p)
}

// ApplyPreset 套用内置预设。
func (t *Tracker) ApplyPreset(id string) (Policy, error) {
	if t == nil {
		return DefaultPolicy(), fmt.Errorf("模型限流映射未启用")
	}
	p, err := ApplyPreset(t.Policy(), id)
	if err != nil {
		return t.Policy(), err
	}
	return t.Save(p)
}

// Reset 回到出厂口径（跟随上游）。
func (t *Tracker) Reset() (Policy, error) {
	if t == nil {
		return DefaultPolicy(), fmt.Errorf("模型限流映射未启用")
	}
	return t.Save(DefaultPolicy())
}

// Decide 返回该 (域, 模型) 此刻的判定。零值 = 放行；未启用或行不存在也放行
// （限流是加性能力，不能因为查不到就把请求拦住）。
func (t *Tracker) Decide(realm, model string) Decision {
	if t == nil {
		return Decision{}
	}
	tbl := t.decisions.Load()
	if tbl == nil {
		return Decision{}
	}
	if d, ok := (*tbl)[decideKey(realm, model)]; ok {
		return d
	}
	return Decision{}
}

// NextChainTarget 给上游 6004 用：返回链上第一个还没试过的模型（空 = 没有下一级）。
// tried 由调用方维护，避免多级降级在同一轮里原地打转。
func (t *Tracker) NextChainTarget(realm, model string, tried map[string]bool) string {
	steps, _ := t.ChainOf(realm, model)
	for _, s := range steps {
		if tried[s.Model] || sameModel(s.Model, model) {
			continue
		}
		return s.Model
	}
	return ""
}

// ChainOf 返回该 (域, 模型) 的降级链与来源。
func (t *Tracker) ChainOf(realm, model string) ([]Step, string) {
	if t == nil {
		return nil, ChainSourceNone
	}
	tbl := t.chains.Load()
	if tbl == nil {
		return nil, ChainSourceNone
	}
	if row, ok := (*tbl)[decideKey(realm, model)]; ok {
		return row.steps, row.source
	}
	return nil, ChainSourceNone
}

// Preview 用当前策略预览某 (域, 模型)。
func (t *Tracker) Preview(realm, model string) Preview {
	if t == nil {
		return Preview{Realm: realm, Model: model}
	}
	return PreviewWith(t.Policy(), t.cfg.Deps, realm, model)
}

// PreviewPolicy 用一份未落盘的候选策略预览（页面的「按上游推算」按钮）。
func (t *Tracker) PreviewPolicy(p Policy, realm, model string) Preview {
	if t == nil {
		return Preview{Realm: realm, Model: model}
	}
	return PreviewWith(p, t.cfg.Deps, realm, model)
}

// Export 生成「提交给上游」的策略包。
func (t *Tracker) Export() Bundle {
	if t == nil {
		return Bundle{Version: 1, Policy: DefaultPolicy()}
	}
	return BuildBundle(t.Policy(), t.View())
}

func decideKey(realm, model string) string {
	return normalizeRealm(realm) + "/" + strings.ToLower(strings.TrimSpace(model))
}

func sameScope(a, b ModelLimit) bool {
	return strings.EqualFold(strings.TrimSpace(a.Realm), strings.TrimSpace(b.Realm)) &&
		strings.EqualFold(strings.TrimSpace(a.Model), strings.TrimSpace(b.Model))
}

func clonePolicy(p Policy) Policy {
	out := p
	if len(p.Overrides) > 0 {
		out.Overrides = make([]ModelLimit, len(p.Overrides))
		for i, o := range p.Overrides {
			if len(o.Chain) > 0 {
				o.Chain = append([]string(nil), o.Chain...)
			}
			out.Overrides[i] = o
		}
	}
	if len(p.Default.Chain) > 0 {
		out.Default.Chain = append([]string(nil), p.Default.Chain...)
	}
	return out
}

func l2p(l ModelLimit) Policy {
	return Policy{Default: Normalize(l)}
}
