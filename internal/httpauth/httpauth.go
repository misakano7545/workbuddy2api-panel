// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
//
// 本包后半部分（Store 起）是**推理面命名 key 注册表**：多把 key、每把带四维
// 策略（模型白名单 / RPM / 并发 / 每 CST 自然日积分预算），另有到期、禁用与
// 轮换宽限。主口令（config api_key）仍由 VerifyBearer 校验，且仍是唯一能进
// 面板与管理端点的钥匙。
package httpauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
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

// TokenMatches 常量时间比较一枚已解析的 token 与期望密钥。
// key 为空恒 false（主口令为空时调用方不应走到这里）。
func TokenMatches(token, key string) bool {
	if key == "" {
		return false
	}
	return subtle.ConstantTimeCompare(digest(token), digest(key)) == 1
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ---------------------------------------------------------------------------
// 命名 API Key 注册表：多 key + 每 key 策略
//
// 与 VerifyBearer 的关系：VerifyBearer 校验的是**主口令**（config api_key），
// 它仍是唯一的全权钥匙——面板、/status、/metrics、/admin 只认它。本节管的是
// 推理面（/v1/*、/v2/chat/busy）的命名 key：可多把、每把四维策略、可到期、
// 可禁用、可轮换。
//
// 为什么放在 httpauth 而不是另起一个包：server 与 panel 必须共用同一套摘要与
// 常量时间比较口径，而本包已经是两者共享的鉴权原语层、且不反向依赖它们；
// 新增一个平级包只会多一份哈希实现和一条依赖边。依赖全部是叶子包
// （cst / livecfg / safefile），不存在环。
//
// 口径：
//   - 只存 SHA-256 摘要，明文 secret 只在创建/轮换的响应里出现一次；
//   - 到期/禁用逐请求判定；轮换后旧 secret 在宽限期内仍可用；
//   - 运行期计数（60s 窗口 / 在途 / 当日积分）只驻内存，重启清零；
//   - 落盘与 state.json 同目录，safefile.WriteKeep 原子替换（含 .bak）、0600。
// ---------------------------------------------------------------------------

const (
	// DefaultRotateGrace 轮换后旧 secret 的默认宽限期。
	DefaultRotateGrace = 24 * time.Hour
	secretPrefix       = "wb2a_"
	keyFileVersion     = 1
)

var (
	// ErrUnauthorized secret 不匹配（含已删除的 key）。
	ErrUnauthorized = errors.New("invalid api key")
	// ErrExpired expires_at 已到点。
	ErrExpired = errors.New("api key expired")
	// ErrDisabled 面板显式禁用。
	ErrDisabled = errors.New("api key disabled")
	// ErrRateLimited 60s 窗口内请求数超限。
	ErrRateLimited = errors.New("api key rate limit exceeded")
	// ErrConcurrent 同时在途请求数超限。
	ErrConcurrent = errors.New("api key concurrency limit exceeded")
	// ErrNotFound 管理面按 ID 找不到 key。
	ErrNotFound = errors.New("api key not found")
)

// Policy 每 key 策略。零值 = 不限。
type Policy struct {
	// Models 与 config models.allowed 同语法：裸名两域生效，"cn:"/"global:" 只该域。
	Models        []string `json:"models,omitempty"`
	RPM           int      `json:"rpm,omitempty"`
	MaxConcurrent int      `json:"max_concurrent,omitempty"`
	DailyCredits  float64  `json:"daily_credits,omitempty"`
}

// Key 持久化定义。Hash 是 secret 的 SHA-256（hex），明文不落盘。
type Key struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Hash      string    `json:"hash"`
	Hint      string    `json:"hint"`
	Policy    Policy    `json:"policy"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Disabled  bool      `json:"disabled,omitempty"`
	// 轮换宽限：PrevHash 在 PrevUntil 之前仍认。
	PrevHash  string    `json:"prev_hash,omitempty"`
	PrevUntil time.Time `json:"prev_until,omitempty"`
	RotatedAt time.Time `json:"rotated_at,omitempty"`
}

// View 对外视图：绝不含 Hash/PrevHash。
type View struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hint      string    `json:"hint"`
	Policy    Policy    `json:"policy"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Disabled  bool      `json:"disabled,omitempty"`
	RotatedAt time.Time `json:"rotated_at,omitempty"`
}

func (k Key) view() View {
	return View{
		ID: k.ID, Name: k.Name, Hint: k.Hint, Policy: k.Policy,
		CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt,
		Disabled: k.Disabled, RotatedAt: k.RotatedAt,
	}
}

type keyFile struct {
	Version int   `json:"version"`
	Keys    []Key `json:"keys"`
}

// UpdateInput 局部更新：nil 字段保持不变；ClearExpiry 显式清空到期时间。
type UpdateInput struct {
	Name        *string
	Policy      *Policy
	ExpiresAt   *time.Time
	ClearExpiry bool
	Disabled    *bool
}

// runtimeState 每 key 的进程内计数（不落盘、重启清零）。
type runtimeState struct {
	mu       sync.Mutex
	inflight int
	winStart time.Time
	winCount int
	day      string
	credits  float64
}

// rollLocked 跨 CST 自然日时清零当日积分。调用方须持 r.mu。
func (r *runtimeState) rollLocked(now time.Time) {
	if day := cst.Day(now); day != r.day {
		r.day = day
		r.credits = 0
	}
}

// Store 注册表。keys 为持久化定义；rt 为运行期计数，按 key ID 索引。
type Store struct {
	path string

	mu   sync.Mutex
	keys []Key
	rt   map[string]*runtimeState
}

// NewKeyStore 打开（或惰性创建）注册表文件。文件不存在 = 空表，不报错。
func NewKeyStore(path string) (*Store, error) {
	s := &Store{path: path, rt: map[string]*runtimeState{}}
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Load 重新读盘（进程内 CRUD 不调用；供测试与运维显式重载）。
func (s *Store) Load() error {
	if s == nil {
		return nil
	}
	raw, err := safefile.Read(s.path, nil)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f keyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	if f.Version > keyFileVersion {
		return fmt.Errorf("%s: 版本 %d 高于本程序支持的 %d（请升级网关）", s.path, f.Version, keyFileVersion)
	}
	s.mu.Lock()
	s.keys = f.Keys
	s.mu.Unlock()
	return nil
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(keyFile{Version: keyFileVersion, Keys: s.keys}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return safefile.WriteKeep(s.path, raw, 0o600)
}

func (s *Store) indexLocked(id string) int {
	for i := range s.keys {
		if s.keys[i].ID == id {
			return i
		}
	}
	return -1
}

// Len 命名 key 数量；nil 安全（未注入注册表）。
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// Empty 报告是否有命名 key；nil 安全。
func (s *Store) Empty() bool { return s.Len() == 0 }

// List 返回全部 key 的安全视图（不含摘要），按创建时间升序。
func (s *Store) List() []View {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]View, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Create 生成一把新 key，返回视图与明文 secret（仅此一次）。
func (s *Store) Create(name string, pol Policy, expires time.Time) (View, string, error) {
	if s == nil {
		return View{}, "", errors.New("api key store not available")
	}
	pol, err := normalizePolicy(pol)
	if err != nil {
		return View{}, "", err
	}
	secret, err := newSecret()
	if err != nil {
		return View{}, "", err
	}
	k := Key{
		ID: newID(), Name: strings.TrimSpace(name), Hash: hashToken(secret),
		Hint: hint(secret), Policy: pol, CreatedAt: time.Now(),
	}
	if !expires.IsZero() {
		k.ExpiresAt = expires
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	if err := s.saveLocked(); err != nil {
		s.keys = s.keys[:len(s.keys)-1]
		return View{}, "", err
	}
	return k.view(), secret, nil
}

// Update 局部更新 name/policy/expires/disabled。落盘失败回滚内存。
func (s *Store) Update(id string, in UpdateInput) (View, error) {
	if s == nil {
		return View{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return View{}, ErrNotFound
	}
	old := s.keys[i]
	k := old
	if in.Name != nil {
		k.Name = strings.TrimSpace(*in.Name)
	}
	if in.Policy != nil {
		pol, err := normalizePolicy(*in.Policy)
		if err != nil {
			return View{}, err
		}
		k.Policy = pol
	}
	if in.ClearExpiry {
		k.ExpiresAt = time.Time{}
	} else if in.ExpiresAt != nil {
		k.ExpiresAt = *in.ExpiresAt
	}
	if in.Disabled != nil {
		k.Disabled = *in.Disabled
	}
	s.keys[i] = k
	if err := s.saveLocked(); err != nil {
		s.keys[i] = old
		return View{}, err
	}
	return k.view(), nil
}

// Delete 删除 key 并清掉其运行期计数。
func (s *Store) Delete(id string) error {
	if s == nil {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return ErrNotFound
	}
	old := s.keys
	s.keys = append(append([]Key{}, s.keys[:i]...), s.keys[i+1:]...)
	if err := s.saveLocked(); err != nil {
		s.keys = old
		return err
	}
	delete(s.rt, id)
	return nil
}

// Rotate 生成新 secret；旧 secret 在 grace 内仍可用（grace<=0 用默认 24h）。
func (s *Store) Rotate(id string, grace time.Duration) (View, string, error) {
	if s == nil {
		return View{}, "", ErrNotFound
	}
	if grace <= 0 {
		grace = DefaultRotateGrace
	}
	secret, err := newSecret()
	if err != nil {
		return View{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return View{}, "", ErrNotFound
	}
	old := s.keys[i]
	k := old
	k.PrevHash = old.Hash
	k.PrevUntil = time.Now().Add(grace)
	k.Hash = hashToken(secret)
	k.Hint = hint(secret)
	k.RotatedAt = time.Now()
	s.keys[i] = k
	if err := s.saveLocked(); err != nil {
		s.keys[i] = old
		return View{}, "", err
	}
	return k.view(), secret, nil
}

// Principal 一次请求的 key 身份 + 运行期句柄。
type Principal struct {
	KeyID  string
	Name   string
	Policy Policy

	models map[string]struct{}
	rt     *runtimeState
	slot   bool
}

var masterPrincipal = &Principal{KeyID: "master", Name: "master"}

// MasterPrincipal 主口令（config api_key）身份：不受任何 per-key 策略约束。
// 共享指针是安全的：它没有 rt，Admit/Charge/Release 全部 no-op。
func MasterPrincipal() *Principal { return masterPrincipal }

// Authenticate 按 secret 查 key：先匹配当前摘要，再匹配未过期的轮换前摘要。
// 常量时间比较；命中后查到期/禁用，并解析模型白名单。
func (s *Store) Authenticate(token string, now time.Time) (*Principal, error) {
	if s == nil || token == "" {
		return nil, ErrUnauthorized
	}
	h := hashToken(token)
	s.mu.Lock()
	var found *Key
	for i := range s.keys {
		k := &s.keys[i]
		if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(h)) == 1 {
			found = k
			break
		}
		if k.PrevHash != "" && now.Before(k.PrevUntil) &&
			subtle.ConstantTimeCompare([]byte(k.PrevHash), []byte(h)) == 1 {
			found = k
			break
		}
	}
	if found == nil {
		s.mu.Unlock()
		return nil, ErrUnauthorized
	}
	k := *found
	st := s.rt[k.ID]
	if st == nil {
		st = &runtimeState{}
		s.rt[k.ID] = st
	}
	s.mu.Unlock()

	switch {
	case k.Disabled:
		return nil, ErrDisabled
	case !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt):
		return nil, ErrExpired
	}
	models, _ := livecfg.NormalizeAllowedModels(k.Policy.Models)
	return &Principal{
		KeyID: k.ID, Name: k.Name, Policy: k.Policy, models: models, rt: st,
	}, nil
}

// Restricted 报告是否配置了模型白名单。
func (p *Principal) Restricted() bool { return p != nil && len(p.models) > 0 }

// Allows 模型白名单判定；未配置白名单（含主口令）= 全放行。
// 与全局 models.allowed 是交集语义：调用方两处都要判。
func (p *Principal) Allows(realm, bare string) bool {
	if p == nil || len(p.models) == 0 {
		return true
	}
	if bare == "" {
		return false
	}
	_, ok := p.models[realm+":"+bare]
	return ok
}

// Admit 过 RPM 与并发闸；成功后需由调用方 defer Release()。
func (p *Principal) Admit(now time.Time) error {
	if p == nil || p.rt == nil {
		return nil
	}
	if p.Policy.RPM > 0 {
		p.rt.mu.Lock()
		if p.rt.winStart.IsZero() || now.Sub(p.rt.winStart) >= time.Minute {
			p.rt.winStart, p.rt.winCount = now, 0
		}
		p.rt.winCount++
		over := p.rt.winCount > p.Policy.RPM
		p.rt.mu.Unlock()
		if over {
			return ErrRateLimited
		}
	}
	if p.Policy.MaxConcurrent > 0 {
		p.rt.mu.Lock()
		if p.rt.inflight >= p.Policy.MaxConcurrent {
			p.rt.mu.Unlock()
			return ErrConcurrent
		}
		p.rt.inflight++
		p.rt.mu.Unlock()
		p.slot = true
	}
	return nil
}

// Release 归还并发槽（幂等）。
func (p *Principal) Release() {
	if p == nil || p.rt == nil || !p.slot {
		return
	}
	p.slot = false
	p.rt.mu.Lock()
	if p.rt.inflight > 0 {
		p.rt.inflight--
	}
	p.rt.mu.Unlock()
}

// AdmitBudget 当日积分准入。未配置预算 = 恒放行。
func (p *Principal) AdmitBudget(now time.Time) (used, limit float64, ok bool) {
	if p == nil || p.rt == nil || p.Policy.DailyCredits <= 0 {
		return 0, 0, true
	}
	p.rt.mu.Lock()
	defer p.rt.mu.Unlock()
	p.rt.rollLocked(now)
	return p.rt.credits, p.Policy.DailyCredits, p.rt.credits < p.Policy.DailyCredits
}

// Charge 回写一次真实扣费（credit 来自上游 usage；ok=false 或 <=0 忽略）。
// 只在配置了日预算时累计——没配预算的 key 不需要这份账。
func (p *Principal) Charge(credit float64, ok bool, now time.Time) {
	if p == nil || p.rt == nil || !ok || credit <= 0 || p.Policy.DailyCredits <= 0 {
		return
	}
	p.rt.mu.Lock()
	p.rt.rollLocked(now)
	p.rt.credits += credit
	p.rt.mu.Unlock()
}

// normalizePolicy 校验策略取值并归一化模型条目（去空白、去空、去重，保持顺序）。
func normalizePolicy(p Policy) (Policy, error) {
	if p.RPM < 0 {
		return Policy{}, fmt.Errorf("policy.rpm: %d 不得为负（0 = 不限）", p.RPM)
	}
	if p.MaxConcurrent < 0 {
		return Policy{}, fmt.Errorf("policy.max_concurrent: %d 不得为负（0 = 不限）", p.MaxConcurrent)
	}
	if math.IsNaN(p.DailyCredits) || math.IsInf(p.DailyCredits, 0) || p.DailyCredits < 0 {
		return Policy{}, fmt.Errorf("policy.daily_credits: %v 非法（须为 >=0 的有限数，0 = 不限）", p.DailyCredits)
	}
	if len(p.Models) > 0 {
		out := make([]string, 0, len(p.Models))
		seen := make(map[string]struct{}, len(p.Models))
		for _, m := range p.Models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
		p.Models = out
	}
	return p, nil
}

// hashToken secret → SHA-256 hex（落盘与比对唯一使用的形式）。
func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// newSecret 生成 wb2a_ + 32 字节随机（base64url，无填充）。
func newSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api key secret: %w", err)
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// newID 生成稳定可读的 key ID（key_ + 12 hex）。
func newID() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		// 随机源失败时退化为时间戳，宁可 ID 不够随机也不能让创建失败：
		// ID 只是管理面标识，真正决定权限的是 secret 摘要。
		return "key_" + fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return "key_" + hex.EncodeToString(raw)
}

// hint 明文提示（列表页展示用）：wb2a_abcd…wxyz，不足以还原 secret。
func hint(secret string) string {
	body := strings.TrimPrefix(secret, secretPrefix)
	if len(body) <= 8 {
		return secretPrefix + "…"
	}
	return secretPrefix + body[:4] + "…" + body[len(body)-4:]
}
