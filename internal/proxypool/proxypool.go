// Package proxypool 出站代理池：按账号绑定固定出口 IP，缓解上游按 IP 限流（429）。
//
// 为什么需要：上游按「出口 IP」计数限流，而不是按账号。本服务所有账号默认从
// 服务器单一公网 IP 出站，池子一开量就撞 429（上游 Issue #3 已确认）。给每个
// 账号分配一个固定代理，等价于把「一个 IP 扛 N 个号」拆成「一个 IP 扛少量号」。
//
// 三种策略（config: upstream.proxy_mode）：
//   - off         : 直连，行为与历史版本完全一致（默认值，升级零行为变化）
//   - per_account : 单号单代理。同一个账号恒定走同一个出口——最贴近真人，
//     账号 IP 不会跳变，是本功能的主推模式
//   - round_robin : 每请求轮换。仅用于「出口都很干净、只求摊薄」的场景；
//     账号 IP 跳变本身可能是风控信号，非必要不用
//
// 健康度：每个出口记录成功/失败计数，连续失败达阈值即标记 dead 并跳过；
// dead 可由面板手动重置。代理失效是常态（免费代理尤甚），自动跳过比让请求
// 卡在死出口上更重要。
package proxypool

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
)

// Mode 代理分配策略。
type Mode string

const (
	// ModeOff 直连，不启用代理池。
	ModeOff Mode = "off"
	// ModePerAccount 单号单代理：同一账号恒定同一出口。
	ModePerAccount Mode = "per_account"
	// ModeRoundRobin 每请求轮换出口。
	ModeRoundRobin Mode = "round_robin"
)

// deadThreshold 连续失败多少次后把出口标记为 dead（跳过不再使用）。
// 取 3：单次失败可能是偶发网络抖动，连续 3 次基本可判出口不可用。
const deadThreshold = 3

// ErrNoProxy 池中没有可用出口（全部 dead 或未配置）。调用方应回落直连。
var ErrNoProxy = errors.New("proxypool: no available proxy")

// entry 单个出口。
type entry struct {
	raw  string // 原始配置串（含账密，仅内部使用，绝不回显明文密码）
	url  *url.URL
	ok   atomic.Int64
	fail atomic.Int64 // 连续失败次数（成功即清零）
	dead atomic.Bool
}

// Info 出口快照（供面板展示；password 字段脱敏）。
type Info struct {
	Raw      string `json:"raw"` // 脱敏后的展示串
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	OK       int64  `json:"ok"`
	Fail     int64  `json:"fail"`
	Dead     bool   `json:"dead"`
	Accounts int    `json:"accounts"` // per_account 模式下绑定的账号数
}

// Pool 代理池。零值不可用，请用 New()。
type Pool struct {
	mu      sync.RWMutex
	entries []*entry
	byRaw   map[string]*entry
	mode    Mode
	cursor  atomic.Uint64
	assign  map[string]*entry // uid -> entry（per_account 模式）
}

// New 构造空池（off 模式）。
func New() *Pool {
	return &Pool{
		byRaw:  make(map[string]*entry),
		assign: make(map[string]*entry),
		mode:   ModeOff,
	}
}

// Configure 用配置列表重建池。list 为空等价于 off。
// 解析失败的条目被跳过并计入返回值 skipped（便于面板提示用户）。
// 已存在且 raw 相同的条目会保留其健康计数，避免每次热更新都清零。
func (p *Pool) Configure(list []string, mode string) (skipped []string) {
	m := Mode(strings.TrimSpace(strings.ToLower(mode)))
	switch m {
	case ModePerAccount, ModeRoundRobin:
	default:
		m = ModeOff
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	old := p.byRaw
	next := make([]*entry, 0, len(list))
	byRaw := make(map[string]*entry, len(list))
	seen := make(map[string]bool, len(list))

	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			continue
		}
		u, err := parseProxy(raw)
		if err != nil {
			skipped = append(skipped, raw)
			continue
		}
		seen[raw] = true
		if e, ok := old[raw]; ok {
			next = append(next, e)
			byRaw[raw] = e
			continue
		}
		e := &entry{raw: raw, url: u}
		next = append(next, e)
		byRaw[raw] = e
	}

	p.entries = next
	p.byRaw = byRaw
	p.mode = m
	if m == ModeOff || len(next) == 0 {
		p.assign = make(map[string]*entry)
	} else {
		// 清掉指向已移除出口的绑定
		for uid, e := range p.assign {
			if byRaw[e.raw] == nil {
				delete(p.assign, uid)
			}
		}
	}
	return skipped
}

// parseProxy 解析代理串。支持 http / https / socks5 / socks5h，
// 省略 scheme 时按 http 处理。
func parseProxy(raw string) (*url.URL, error) {
	s := raw
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("unsupported scheme " + u.Scheme)
	}
	return u, nil
}

// Mode 返回当前策略。
func (p *Pool) Mode() Mode {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode
}

// Len 返回出口总数。
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries)
}

// Pick 为 uid 选一个出口。返回 (nil, "") 表示「应直连」——
// off 模式、池空、或全部 dead。
func (p *Pool) Pick(uid string) (*url.URL, string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.mode == ModeOff || len(p.entries) == 0 {
		return nil, ""
	}

	if p.mode == ModePerAccount {
		if uid != "" {
			if e, ok := p.assign[uid]; ok && !e.dead.Load() {
				return e.url, e.raw
			}
		}
		e := p.pickHealthyLocked()
		if e == nil {
			return nil, ""
		}
		if uid != "" {
			p.assign[uid] = e
		}
		return e.url, e.raw
	}

	// ModeRoundRobin
	e := p.pickHealthyLocked()
	if e == nil {
		return nil, ""
	}
	return e.url, e.raw
}

// pickHealthyLocked 按游标轮转找一个非 dead 出口。调用方须持写锁。
func (p *Pool) pickHealthyLocked() *entry {
	n := len(p.entries)
	if n == 0 {
		return nil
	}
	start := int(p.cursor.Add(1)-1) % n
	for i := 0; i < n; i++ {
		e := p.entries[(start+i)%n]
		if !e.dead.Load() {
			return e
		}
	}
	return nil
}

// Peek 返回 uid 当前已绑定的出口 raw，**不做任何分配**（未绑定返回空串）。
//
// 用途：Transport.Proxy 只负责选路，拿不到请求成败；健康度必须在请求出口处
// （doJSON / ChatStreamContext）回写。那里只有 *http.Request，只能靠 uid 反查
// 「本次实际走的哪个出口」——用 Pick 反查会连带做分配，属副作用，故单列 Peek。
//
// 仅 per_account 模式有绑定表，round_robin / off 恒返回空串（该模式不统计健康度）。
func (p *Pool) Peek(uid string) string {
	if uid == "" {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.assign[uid]
	if !ok || e == nil {
		return ""
	}
	return e.raw
}

// Report 上报一次出口使用结果。成功清零失败计数；失败累加，达阈值标记 dead。
func (p *Pool) Report(raw string, ok bool) {
	if raw == "" {
		return
	}
	p.mu.RLock()
	e := p.byRaw[raw]
	p.mu.RUnlock()
	if e == nil {
		return
	}
	if ok {
		e.ok.Add(1)
		e.fail.Store(0)
		if e.dead.Load() {
			e.dead.Store(false)
		}
		return
	}
	if e.fail.Add(1) >= deadThreshold {
		e.dead.Store(true)
	}
}

// ResetDead 把所有出口的 dead 标记与失败计数清零（面板「重置健康度」）。
func (p *Pool) ResetDead() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, e := range p.entries {
		if e.dead.Load() {
			n++
		}
		e.dead.Store(false)
		e.fail.Store(0)
	}
	return n
}

// ResetAssign 清空账号与出口的绑定，下次请求重新分配。
func (p *Pool) ResetAssign() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.assign = make(map[string]*entry)
}

// Snapshot 返回出口快照（密码脱敏）。
func (p *Pool) Snapshot() []Info {
	p.mu.RLock()
	defer p.mu.RUnlock()

	bound := make(map[string]int, len(p.entries))
	for _, e := range p.assign {
		bound[e.raw]++
	}

	out := make([]Info, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, Info{
			Raw:      maskProxy(e.raw),
			Scheme:   strings.ToLower(e.url.Scheme),
			Host:     e.url.Host,
			OK:       e.ok.Load(),
			Fail:     e.fail.Load(),
			Dead:     e.dead.Load(),
			Accounts: bound[e.raw],
		})
	}
	return out
}

// maskProxy 脱敏：保留 scheme 与 host，userinfo 里的密码替换为 ***。
func maskProxy(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	scheme, rest := raw[:i], raw[i+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return raw
	}
	userinfo := rest[:at]
	host := rest[at+1:]
	if c := strings.Index(userinfo, ":"); c >= 0 {
		userinfo = userinfo[:c] + ":***"
	}
	return scheme + "://" + userinfo + "@" + host
}

// ── context 传递账号 uid ────────────────────────────────────────────
//
// Transport.Proxy 是 func(*http.Request)，拿不到业务层的 *auth.Auth，
// 因此由请求构造处把 uid 塞进 context，Proxy 再从 context 取。

type ctxKey struct{}

// WithUID 把账号 uid 绑进 context。
func WithUID(ctx context.Context, uid string) context.Context {
	if uid == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, uid)
}

// UIDFrom 从 context 取账号 uid（取不到返回空串）。
func UIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// ── 进程级默认池 ────────────────────────────────────────────────────

var defaultPool = New()

// Default 返回进程级默认池（transport 与 panel 共用同一实例）。
func Default() *Pool { return defaultPool }
