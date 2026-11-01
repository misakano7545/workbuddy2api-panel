// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
//
// 除了管理员密钥（config.api_key，单把、不计量），本包还承载客户端密钥表：
// 管理员在面板里新建、发给别人调 /v1/* 的多把密钥，带启用开关、到期时间、
// 日/总量上限（请求数、token、积分）与模型白名单。两者语义严格分开——
// 管理员密钥能登面板、调全部接口且不计量；客户端密钥只能调 /v1/*，撞运维端点
// 一律 403。
package httpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// BearerToken 取出 Authorization: Bearer <token> 里的 token（缺头/方案不符返回空串）。
//
// 客户端密钥需要拿到原文去和密钥表比对（不能只做「等于管理员密钥」的常量时间
// 比较），所以把解析单独暴露；VerifyBearer 仍走 digest 比较，口径不变。
func BearerToken(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		return ""
	}
	return strings.TrimSpace(authz[len(bearerPrefix):])
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ---------------------------------------------------------------------------
// 客户端密钥表
//
// 落盘：state 同目录 api_keys.json（0600，safefile 原子替换 + .bak），密钥原文也
// 在文件里（面板要能随时复制）。增删改立即写盘，写不成就撤回；用量计数防抖写盘
// （flushInterval），Stop 时再写一次。文件不存在 = 未启用——这条很重要：
// 「一个客户端密钥都没建」必须与「文件读坏了」区分开，后者一律报错。
// ---------------------------------------------------------------------------

// flushInterval 用量计数的防抖落盘间隔（var 供测试缩短）。
var flushInterval = 10 * time.Second

// randRead 生成密钥与 id 的随机源（测试可替换成失败实现）。
var randRead = rand.Read

const (
	keyFileVersion = 1
	maxKeyName     = 64
	maxKeyNote     = 500
	maxKeyModels   = 100
	maxKeyPattern  = 128
)

// Limits 一组上限，0 表示这一项不限。
type Limits struct {
	Requests int64   `json:"requests,omitempty"`
	Tokens   int64   `json:"tokens,omitempty"`
	Credits  float64 `json:"credits,omitempty"`
}

// Usage 一段时间里的用量。
//
// Requests 在准入时先加、最终 4xx/5xx 退还（Refund），所以只算成功请求；
// Tokens/Credits 按上游末帧 usage 累加，换号重试各次都算（上游确实消耗了）。
type Usage struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Credits  float64 `json:"credits"`
}

// ClientKey 一把客户端密钥。
type ClientKey struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Note       string    `json:"note,omitempty"`
	Secret     string    `json:"secret"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"` // 零值 = 不过期
	DailyLimit Limits    `json:"daily_limit"`
	TotalLimit Limits    `json:"total_limit"`
	// Models 模型白名单：裸模型名，支持 * 通配，大小写不敏感；空 = 不限。
	// 匹配的是别名映射/降级之后实际发往上游的模型名。
	Models     []string  `json:"models,omitempty"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	Used       Usage     `json:"used"`
	Today      Usage     `json:"today"`
	Day        string    `json:"day,omitempty"` // 北京时间 2006-01-02
}

// ClientKeyLease 一次已准入请求的只读快照：计量按 ID 回查，模型白名单在准入时
// 冻结，避免请求中途改密钥导致行为漂移，也避免跨 goroutine 读 *ClientKey 字段。
type ClientKeyLease struct {
	ID     string
	Name   string
	models []string
}

// AllowsModel 白名单是否放行这个模型（裸名）。空白名单 = 放行。
func (l *ClientKeyLease) AllowsModel(model string) bool {
	if l == nil || len(l.models) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, p := range l.models {
		if wildcardMatch(strings.ToLower(p), model) {
			return true
		}
	}
	return false
}

// ClientKeyDenied 这次请求不能用：停用、过期、没这把密钥，或用到了上限。
type ClientKeyDenied struct {
	Status  int    // 401 / 403 / 429
	Code    string // key_disabled / key_expired / key_quota_exceeded / invalid_api_key
	Message string
	// RetryAfter 日限用完时到 CST 次日 00:00 的剩余时间；总量用完为 0（等不来）。
	RetryAfter time.Duration
}

func (d *ClientKeyDenied) Error() string { return d.Message }

// ErrNoSuchClientKey 没有这个 id 的密钥。
var ErrNoSuchClientKey = errors.New("密钥不存在")

// ClientKeyInput 新建/修改密钥时可设的字段。
type ClientKeyInput struct {
	Name       string    `json:"name"`
	Note       string    `json:"note"`
	Enabled    bool      `json:"enabled"`
	ExpiresAt  time.Time `json:"expires_at"`
	DailyLimit Limits    `json:"daily_limit"`
	TotalLimit Limits    `json:"total_limit"`
	Models     []string  `json:"models"`
}

// clientKeyCtxKey 请求级 lease 的 context 键。
type clientKeyCtxKey struct{}

// WithClientKey 把已准入的密钥快照挂进请求 context（server 中间件用）。
func WithClientKey(ctx context.Context, l *ClientKeyLease) context.Context {
	return context.WithValue(ctx, clientKeyCtxKey{}, l)
}

// ClientKeyFrom 取请求 context 里的密钥快照；无（管理员密钥/未启用）返回 nil。
func ClientKeyFrom(ctx context.Context) *ClientKeyLease {
	l, _ := ctx.Value(clientKeyCtxKey{}).(*ClientKeyLease)
	return l
}

// ClientKeyStore 客户端密钥存储，并发安全。
type ClientKeyStore struct {
	path string

	// mu 护住全部字段，写盘也在锁内：文件只有几 KB，写一次不到一毫秒，换来
	// 「盘上顺序 = 内存改动顺序」，不会让旧的用量快照盖掉刚写下的增删改。
	mu       sync.Mutex
	keys     []*ClientKey          // 按创建顺序
	bySecret map[string]*ClientKey // 密钥原文的 SHA-256 → 密钥
	dirty    bool                  // 有未写盘的用量计数

	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

type clientKeyFile struct {
	Version int          `json:"version"`
	Keys    []*ClientKey `json:"keys"`
}

// OpenClientKeys 读取 path 下的密钥文件。文件不存在 = 从空开始（未启用）；读不了
// 或格式不对时报错——不能当成「没有密钥」，否则「建过密钥就必须带」的规则会悄悄
// 失效。path 为空时只存在内存里（测试用）。
//
// 返回值恒非 nil（出错时也为 nil, err），方便调用方直接判 nil store。
func OpenClientKeys(path string) (*ClientKeyStore, error) {
	s := &ClientKeyStore{
		path:     path,
		bySecret: map[string]*ClientKey{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if path == "" {
		return s, nil
	}
	raw, err := safefile.Read(path, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("读取密钥文件 %s: %w", path, err)
	}
	var f clientKeyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析密钥文件 %s: %w", path, err)
	}
	for _, k := range f.Keys {
		if k == nil || k.ID == "" || k.Secret == "" {
			continue
		}
		s.keys = append(s.keys, k)
		s.bySecret[keyDigest(k.Secret)] = k
	}
	return s, nil
}

// Start 启动用量计数的防抖落盘；Stop 前一直运行（重复调用只起一个 goroutine）。
func (s *ClientKeyStore) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go func() {
		defer close(s.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				s.Flush()
				return
			case <-t.C:
				s.Flush()
			}
		}
	}()
}

// Stop 停止后台落盘并把最后一次计数写盘（幂等）。未 Start 时直接写一次盘
// （done 只有 Start 的 goroutine 会关闭，不能盲等）。
func (s *ClientKeyStore) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
		s.mu.Lock()
		running := s.started
		s.mu.Unlock()
		if running {
			<-s.done
			return
		}
		s.Flush()
	})
}

// Flush 立即写盘（停机路径与测试用）。
func (s *ClientKeyStore) Flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

// HasKeys 是否建过至少一把密钥（server 用它判「必须带密钥」）。
func (s *ClientKeyStore) HasKeys() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys) > 0
}

// List 返回全部密钥的副本（面板展示用；调用方可直接改，不影响存储）。
func (s *ClientKeyStore) List() []ClientKey {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ClientKey, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, *cloneClientKey(k))
	}
	return out
}

// Get 按 id 取一把密钥的副本。
func (s *ClientKeyStore) Get(id string) (ClientKey, bool) {
	if s == nil {
		return ClientKey{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byIDLocked(id)
	if k == nil {
		return ClientKey{}, false
	}
	return *cloneClientKey(k), true
}

// Create 新建一把密钥。写盘失败时内存不变（不返回「已建但没落盘」的半成品）。
func (s *ClientKeyStore) Create(in ClientKeyInput) (ClientKey, error) {
	in, err := normalizeClientKeyInput(in)
	if err != nil {
		return ClientKey{}, err
	}
	idRaw := make([]byte, 8)
	if _, err := randRead(idRaw); err != nil {
		return ClientKey{}, fmt.Errorf("生成密钥 id: %w", err)
	}
	secRaw := make([]byte, 32)
	if _, err := randRead(secRaw); err != nil {
		return ClientKey{}, fmt.Errorf("生成密钥: %w", err)
	}
	k := &ClientKey{
		ID:         hex.EncodeToString(idRaw),
		Name:       in.Name,
		Note:       in.Note,
		Secret:     "sk-" + base64.RawURLEncoding.EncodeToString(secRaw),
		Enabled:    in.Enabled,
		CreatedAt:  time.Now(),
		ExpiresAt:  in.ExpiresAt,
		DailyLimit: in.DailyLimit,
		TotalLimit: in.TotalLimit,
		Models:     in.Models,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append(append([]*ClientKey{}, s.keys...), k)
	if err := s.persistLocked(next); err != nil {
		return ClientKey{}, err
	}
	s.keys = next
	s.bySecret = indexKeys(next)
	s.dirty = false
	return *cloneClientKey(k), nil
}

// Update 按 id 改一把密钥：名称/备注/启用/到期/上限/白名单可改，密钥原文与累计
// 用量不动。
func (s *ClientKeyStore) Update(id string, in ClientKeyInput) (ClientKey, error) {
	in, err := normalizeClientKeyInput(in)
	if err != nil {
		return ClientKey{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.byIDLocked(id)
	if cur == nil {
		return ClientKey{}, ErrNoSuchClientKey
	}
	nk := cloneClientKey(cur)
	nk.Name, nk.Note, nk.Enabled = in.Name, in.Note, in.Enabled
	nk.ExpiresAt = in.ExpiresAt
	nk.DailyLimit, nk.TotalLimit, nk.Models = in.DailyLimit, in.TotalLimit, in.Models
	next := make([]*ClientKey, 0, len(s.keys))
	for _, k := range s.keys {
		if k.ID == id {
			next = append(next, nk)
			continue
		}
		next = append(next, k)
	}
	if err := s.persistLocked(next); err != nil {
		return ClientKey{}, err
	}
	s.keys = next
	s.bySecret = indexKeys(next)
	s.dirty = false
	return *cloneClientKey(nk), nil
}

// Delete 删除一把密钥。
func (s *ClientKeyStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byIDLocked(id) == nil {
		return ErrNoSuchClientKey
	}
	next := make([]*ClientKey, 0, len(s.keys))
	for _, k := range s.keys {
		if k.ID != id {
			next = append(next, k)
		}
	}
	if err := s.persistLocked(next); err != nil {
		return err
	}
	s.keys = next
	s.bySecret = indexKeys(next)
	s.dirty = false
	return nil
}

// Admit 校验一把密钥并预扣一次请求数。
//
// 返回 (lease, nil) = 放行；(nil, denied) = 拒绝；(nil, nil) = 没有可比的密钥表
// （未建过任何密钥，调用方按旧语义处理）。
func (s *ClientKeyStore) Admit(secret string, now time.Time) (*ClientKeyLease, *ClientKeyDenied) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) == 0 {
		return nil, nil
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, invalidKeyDenied()
	}
	k := s.bySecret[keyDigest(secret)]
	if k == nil {
		return nil, invalidKeyDenied()
	}
	s.rollDayLocked(k, now)
	if d := checkLocked(k, now); d != nil {
		return nil, d
	}
	k.Today.Requests++
	k.Used.Requests++
	k.LastUsedAt = now
	s.dirty = true
	return &ClientKeyLease{ID: k.ID, Name: k.Name, models: append([]string(nil), k.Models...)}, nil
}

// AddUsage 累加一次上游调用的 token / credit（token 来自末帧 usage，credit 可能为 0）。
func (s *ClientKeyStore) AddUsage(id string, tokens int64, credit float64) {
	if s == nil || (tokens <= 0 && credit <= 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byIDLocked(id)
	if k == nil {
		return
	}
	s.rollDayLocked(k, time.Now())
	if tokens > 0 {
		k.Today.Tokens += tokens
		k.Used.Tokens += tokens
	}
	if credit > 0 {
		k.Today.Credits += credit
		k.Used.Credits += credit
	}
	s.dirty = true
}

// Refund 退还准入时预扣的这一次请求（最终 4xx/5xx，或客户端密钥撞了运维端点）。
func (s *ClientKeyStore) Refund(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byIDLocked(id)
	if k == nil {
		return
	}
	if k.Today.Requests > 0 {
		k.Today.Requests--
	}
	if k.Used.Requests > 0 {
		k.Used.Requests--
	}
	s.dirty = true
}

// rollDayLocked 跨 CST 自然日时清零 Today（字典序即时间序）。
func (s *ClientKeyStore) rollDayLocked(k *ClientKey, now time.Time) {
	day := cst.Day(now)
	if k.Day == day {
		return
	}
	k.Day = day
	k.Today = Usage{}
}

// checkLocked 判停用/过期/超限。调用方必须已持锁并先 rollDayLocked。
func checkLocked(k *ClientKey, now time.Time) *ClientKeyDenied {
	if !k.Enabled {
		return &ClientKeyDenied{Status: http.StatusUnauthorized, Code: "key_disabled", Message: "API key is disabled"}
	}
	if !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) {
		return &ClientKeyDenied{Status: http.StatusUnauthorized, Code: "key_expired", Message: "API key has expired"}
	}
	if overRequests(k.DailyLimit.Requests, k.Today.Requests) ||
		overRequests(k.DailyLimit.Tokens, k.Today.Tokens) ||
		overCredits(k.DailyLimit.Credits, k.Today.Credits) {
		return &ClientKeyDenied{
			Status:     http.StatusTooManyRequests,
			Code:       "key_quota_exceeded",
			Message:    "daily quota exceeded (resets at 00:00 CST)",
			RetryAfter: untilNextCSTDay(now),
		}
	}
	if overRequests(k.TotalLimit.Requests, k.Used.Requests) ||
		overRequests(k.TotalLimit.Tokens, k.Used.Tokens) ||
		overCredits(k.TotalLimit.Credits, k.Used.Credits) {
		return &ClientKeyDenied{
			Status:  http.StatusTooManyRequests,
			Code:    "key_quota_exceeded",
			Message: "total quota exceeded",
		}
	}
	return nil
}

// overRequests 判「已用 >= 上限」，0 = 不限。
func overRequests(limit, used int64) bool { return limit > 0 && used >= limit }

// overCredits 判「已用 >= 上限」，0 = 不限（负值同样视为不限）。
func overCredits(limit, used float64) bool { return limit > 0 && used >= limit }

// untilNextCSTDay 到 CST 次日 00:00 的剩余时间（日限的 Retry-After）。
func untilNextCSTDay(now time.Time) time.Duration {
	n := now.In(cst.Loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, cst.Loc).AddDate(0, 0, 1)
	d := next.Sub(n)
	if d < 0 {
		return 0
	}
	return d
}

func invalidKeyDenied() *ClientKeyDenied {
	return &ClientKeyDenied{
		Status:  http.StatusUnauthorized,
		Code:    "invalid_api_key",
		Message: "missing or invalid API key",
	}
}

// byIDLocked 按 id 找密钥（无锁内部实现，调用方必须持锁）。
func (s *ClientKeyStore) byIDLocked(id string) *ClientKey {
	if id == "" {
		return nil
	}
	for _, k := range s.keys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// persistLocked 把 next 全量写盘（无锁内部实现，调用方必须持锁）。
func (s *ClientKeyStore) persistLocked(next []*ClientKey) error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(clientKeyFile{Version: keyFileVersion, Keys: next})
	if err != nil {
		return fmt.Errorf("序列化密钥表: %w", err)
	}
	if err := safefile.WriteKeep(s.path, b, 0o600); err != nil {
		return fmt.Errorf("写密钥文件 %s: %w", s.path, err)
	}
	return nil
}

// flushLocked 只在有未落盘计数时写（无锁内部实现，调用方必须持锁）。
func (s *ClientKeyStore) flushLocked() {
	if !s.dirty {
		return
	}
	if err := s.persistLocked(s.keys); err != nil {
		// 计数丢一次不影响服务：下一次改动会再写，且准入仍以内存为准。
		return
	}
	s.dirty = false
}

// indexKeys 重建 密钥摘要 → 密钥 的索引。
func indexKeys(keys []*ClientKey) map[string]*ClientKey {
	m := make(map[string]*ClientKey, len(keys))
	for _, k := range keys {
		if k != nil && k.Secret != "" {
			m[keyDigest(k.Secret)] = k
		}
	}
	return m
}

// cloneClientKey 深拷贝一把密钥（Models 切片独立，避免面板改副本污染存储）。
func cloneClientKey(k *ClientKey) *ClientKey {
	if k == nil {
		return nil
	}
	cp := *k
	cp.Models = append([]string(nil), k.Models...)
	return &cp
}

// keyDigest 密钥原文 → 定长摘要（只用于查表，不落盘）。
func keyDigest(secret string) string { return hex.EncodeToString(digest(secret)) }

// normalizeClientKeyInput 裁剪/校验面板输入，顺带把白名单去重。
func normalizeClientKeyInput(in ClientKeyInput) (ClientKeyInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Note = strings.TrimSpace(in.Note)
	if in.Name == "" {
		return in, errors.New("名称不能为空")
	}
	if utf8.RuneCountInString(in.Name) > maxKeyName {
		return in, fmt.Errorf("名称最长 %d 个字符", maxKeyName)
	}
	if utf8.RuneCountInString(in.Note) > maxKeyNote {
		return in, fmt.Errorf("备注最长 %d 个字符", maxKeyNote)
	}
	for _, l := range []struct {
		name string
		v    Limits
	}{{"每日上限", in.DailyLimit}, {"总量上限", in.TotalLimit}} {
		if l.v.Requests < 0 || l.v.Tokens < 0 || l.v.Credits < 0 {
			return in, fmt.Errorf("%s不能为负", l.name)
		}
	}
	models := make([]string, 0, len(in.Models))
	seen := map[string]bool{}
	for _, m := range in.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if utf8.RuneCountInString(m) > maxKeyPattern {
			return in, fmt.Errorf("模型名最长 %d 个字符", maxKeyPattern)
		}
		key := strings.ToLower(m)
		if seen[key] {
			continue
		}
		seen[key] = true
		models = append(models, m)
	}
	if len(models) > maxKeyModels {
		return in, fmt.Errorf("模型白名单最多 %d 条", maxKeyModels)
	}
	exp := in.ExpiresAt
	if !exp.IsZero() {
		// 只保留秒级精度：面板传毫秒时避免「同一时刻」判过期抖动。
		exp = exp.Truncate(time.Second)
	}
	in.ExpiresAt = exp
	in.Models = models
	return in, nil
}

// wildcardMatch 单串 glob：* 匹配任意长度（含空），其余字符逐字（已转小写）。
func wildcardMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	// 首段必须前缀命中，尾段必须后缀命中，中间段按顺序出现。
	rest := s
	if parts[0] != "" {
		if !strings.HasPrefix(rest, parts[0]) {
			return false
		}
		rest = rest[len(parts[0]):]
	}
	last := parts[len(parts)-1]
	for _, p := range parts[1 : len(parts)-1] {
		if p == "" {
			continue
		}
		idx := strings.Index(rest, p)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(p):]
	}
	if last == "" {
		return true
	}
	return strings.HasSuffix(rest, last) && len(rest) >= len(last)
}
