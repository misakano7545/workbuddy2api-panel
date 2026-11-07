// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// wbSealed 源文件里原本是 $wbEncrypted 信封的字段（"auth.accessToken" 形态）：
	// SaveAtomic 回写时必须逐个复原成信封，否则刷新后的 token 会以明文覆盖掉
	// 桌面端的加密文件。nil = 来源文件没加密。
	wbSealed map[string]bool
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争（go test -race 实证）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// IsEnterprise 报告账号是否为企业版（auth 文件带非空 enterpriseId）。
//
// 企业版**没有个人成长体系**：签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子
// 在上游一律被拒——实测（2026-10-07，同一时刻 A/B 对照）：
//   - POST /v2/billing/meter/daily-checkin                → 400 code 10001「企业账号不支持该操作」
//   - POST /billing/meter/claim-gift / claim-compensation → 400 code 10001 同上
//   - GET  /activity/growth/{streak,buddy/info,heatmap,lottery/summary}
//     与 GET /v2/activity/growth/tasks                    → 403「growth system is only available for personal users」
//
// 因此引用处（scheduler 五类任务 + panel 成长任务/签到入口）必须与 IsGlobal 门控
// 并列跳过，避免每日对注定失败的端点空发请求、面板按钮必然报错。
//
// 不受影响的能力：选号派发、保活（token 刷新）、额度查询（企业额度改走
// /v2/billing/meter/get-enterprise-user-usage，见 upstream.enterpriseResource）。
//
// 直读字段（EnterpriseID 自 Parse 后不再改写，与 upstream 读取方式一致），
// 无需加锁——Realm() 加锁是因为 realm/Domain 会被 RefreshToken 在锁内改写。
func (a *Auth) IsEnterprise() bool { return strings.TrimSpace(a.EnterpriseID) != "" }

// apiTokenPrefixes 静态 API Key 的前缀集合：ck_（CodeBuddy 控制台签发）与
// pt_（官方 CLI @tencent-ai/codebuddy-code 另有该前缀）。
//
// 官方 CLI 的判据是 tokenType === "ApiKey" 且 token 以这两个前缀之一开头；auth
// 文件不持久化 tokenType（refresh 只认 OAuth 设备流，静态 Key 无法刷新），前缀是
// 落盘形态里唯一稳定、跨进程一致的判据。
var apiTokenPrefixes = []string{"ck_", "pt_"}

// isAPIKeyToken 判 token 是否以静态 API Key 前缀开头。
func isAPIKeyToken(at string) bool {
	for _, p := range apiTokenPrefixes {
		if strings.HasPrefix(at, p) {
			return true
		}
	}
	return false
}

// isAPIKeyLocked IsAPIKey 的无锁内部实现：仅限**已持 a.mu** 的调用方使用
// （sync.Mutex 不可重入，锁内再调 AccessTokenValue 会自锁）。
func (a *Auth) isAPIKeyLocked() bool { return isAPIKeyToken(a.AccessToken) }

// IsAPIKey 报告账号是否为静态 API Key 账号（AccessToken 以 ck_/pt_ 开头）。
//
// 静态 Key 直接可用（Authorization: Bearer <key>，官方 CLI 同时发 X-API-Key），
// 但它**没有**个人账号身份：签到 / 成长任务 / 连登 / 旅行 / 夜猫子一律不适用，
// 额度只能走企业口径端点（个人 get-user-resource 对静态 Key 返回空 Accounts），
// 见 UsesEnterpriseBilling。
//
// 经 AccessTokenValue() 加锁读取：RefreshToken 会在锁内改写 AccessToken。
func (a *Auth) IsAPIKey() bool {
	if a == nil {
		return false
	}
	return isAPIKeyToken(a.AccessTokenValue())
}

// UsesEnterpriseBilling 报告账号的额度查询是否该走企业口径端点
// （/v2/billing/meter/get-enterprise-user-usage）。
//
// 企业版与静态 API Key 账号同属此列：个人 get-user-resource 对二者均返回空
// Accounts（企业账号无个人资源包；静态 Key 无个人账号身份），额度必须走企业口径，
// 否则余额恒为 0 → 积分保底把可用号当成耗尽号挡掉。
func (a *Auth) UsesEnterpriseBilling() bool { return a.IsEnterprise() || a.IsAPIKey() }

// TokenSource 从 AccessToken 的 JWT payload 里读 token_source 声明（官方客户端用它
// 填出站 X-Auth-Method 与 X-Userinfo.token_source）。非 JWT / 无该声明 / 解码失败一律
// 返回空串——空表示「这个账号没有该维度」，调用方随之不发对应头（不伪造）。
//
// 每次现解码而不缓存：RefreshToken 会整体换发 token，token_source 可能随重新授权翻转。
// 成本是一次 base64 + 小 JSON，相对一次上游请求可忽略。
func (a *Auth) TokenSource() string {
	if a == nil {
		return ""
	}
	return jwtClaim(a.AccessTokenValue(), "token_source")
}

// IdSource 从 AccessToken 的 JWT payload 里读 `id_source` 声明（官方客户端据此填
// 出站 X-Id-Source 头与 X-Userinfo.id_source 段）。
//
// 与 TokenSource 的差别很重要：token_source 描述「这次授权走了哪条流程」（企业切换等，
// 个人号也有），id_source 描述「身份来源」——两者在真机 runIdentityHeaders() 里是
// 各自独立触发的条件维度。同样现解码不缓存：换发 token 后声明可能变。
func (a *Auth) IdSource() string {
	if a == nil {
		return ""
	}
	return jwtClaim(a.AccessTokenValue(), "id_source")
}

// jwtClaim 解出 JWT payload 里的指定字符串声明；非 JWT / 解码失败返回空。
// 先按无填充 base64url 解（JWT 标准），失败再试带填充形态（兼容非标准签发方）。
func jwtClaim(token, name string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	v, _ := payload[name].(string)
	return strings.TrimSpace(v)
}

// SupportsPersonalGrowth 报告账号是否具备个人成长体系（签到 / 成长任务 / 连登管家 /
// 猫猫旅行 / 夜猫子）。企业版与静态 API Key 都没有该体系：
//   - 企业版：上游一律拒绝（400 code 10001 / 403 growth-only，见 IsEnterprise）；
//   - 静态 API Key：根本没有个人账号身份。
//
// scheduler 各任务与 panel 成长入口统一用本方法门控，避免对注定失败的端点空发请求、
// 把本可正常对话的号攒出一串上游报错。
func (a *Auth) SupportsPersonalGrowth() bool { return !a.IsEnterprise() && !a.IsAPIKey() }

// SupportsPersonalEndpoints 报告账号是否可调用「签到 / 任务列表」这类显式报错的个人端点。
// 与 SupportsPersonalGrowth（执行类门控）刻意分开命名：两者当前的判据相同，但语义不同
// ——将来若某类账号「读得到、记不上账」，只需改这一处而不影响执行类门控。
func (a *Auth) SupportsPersonalEndpoints() bool { return !a.IsEnterprise() && !a.IsAPIKey() }

// 不要再用 AccessToken 的 token_source=enterprise_switch 判定「企业上下文」：
// 实测两个只带该章的**个人版**号（enterpriseId 空）成长任务全部 claimed、行为事件
// 照常计分——该章只记本次授权走了哪条流程。企业形态一律以 enterpriseId 为准。

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return true
	}
	// 官方 CLI 国际站域（product.json externalDomain: www.codebuddy.ai）。与 workbuddy.ai
	// 同后端不同主机名，但 domain 落盘值不同，不认它会把国际站账号误判成 cn
	// （显式 realm 键可兜住面板登录的账号，手写 auth 文件则直接错域路由）。
	return d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	// 静态 API Key 永不刷新（refresh 端点只认 OAuth 设备流）：恒 false，否则请求
	// 路径会先 refresh、失败后把该号当坏号冷却换号——Key 本身是好的。
	if a.isAPIKeyLocked() {
		return false
	}
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	// 新版桌面端把 auth 字段换成 $wbEncrypted 信封（见 wbencrypted.go）。
	// 解密后再按原两形态解析；未加密文件原样返回，不吃这条路径。
	dec, sealed, err := decryptWBEncrypted(raw)
	if err != nil {
		return nil, err
	}
	raw = dec
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	a.ExpiresAt = normalizeExpiresAt(a.ExpiresAt)
	a.wbSealed = sealed
	return &a, nil
}

// expiresAtMillisThreshold 区分「秒」与「毫秒」时间戳的分界（1e11）。
//
// 单位到 1e11 秒已是公元 5138 年，真实凭证不可能；而 1e11 毫秒 = 1973 年，
// 毫秒制当前值（~1.8e12）远在其上。阈值取在两者之间不会误判任一侧。
const expiresAtMillisThreshold = 100_000_000_000

// normalizeExpiresAt 把上游凭据的 expiresAt 归一到 Unix 秒。
//
// 为什么需要：官方 CodeBuddy CLI 的凭据文件
// （%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\*.info）写的是**毫秒**
// （实测 1795254516410），而本仓与旧插件 OAuth 落盘的是**秒**。直接按秒解释毫秒
// 会把到期时间推到公元 58800 年 → NeedsRefresh 恒 false → token 永不刷新 →
// 到期后账号静默失效（表现为「用了一个月突然全部 401」）。导入官方凭据是本仓
// 支持的用法，所以解析入口必须归一，而不是靠使用者手改文件。
func normalizeExpiresAt(v int64) int64 {
	if v > expiresAtMillisThreshold {
		return v / 1000
	}
	return v
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// 来源文件加密的，回写也保持加密：按读入时的信封清单复原（见 sealForSave）。
	// 封装失败就拒绝落盘——明文降级覆盖加密文件比写不进去更糟。
	if err := sealForSave(doc, a.wbSealed); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// 落盘走 safefile：tmp fsync 后才 rename（掉电不会留下长度正确、内容全 0 的
	// 凭证文件），并把上一份有效内容留成 <file>.bak —— LoadDir 读到坏文件时用它。
	if err := safefile.WriteKeep(a.FilePath, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", a.FilePath, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return nil
}

// SaveAtomicRetry 在 SaveAtomic 基础上做有界重试（0/100ms/300ms）。
//
// 为什么需要：refresh 成功后上游已经轮换了 RT，新 token 只在内存里；如果这一次
// 落盘失败（磁盘瞬时满、bind-mount 抖动），进程重启会拿旧 RT 去刷新——旧 RT 可能
// 已被消费，账号直接进入「需要重新登录」。重试把瞬时失败挡掉；仍失败由调用方
// 打 ERR（错误汇总可见），至少不让问题静默。
func (a *Auth) SaveAtomicRetry() error {
	var err error
	for i, delay := range []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond} {
		if i > 0 {
			time.Sleep(delay)
		}
		if err = a.SaveAtomic(); err == nil {
			return nil
		}
	}
	return err
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	// 按 mtime 升序：同一个号的轮转备份并存时（客户端会写 <name>.<ts>.<pid>.<uuid>.info），
	// 池里「后载入者胜出」才等于「最新凭证胜出」。不排序的话 glob 顺序（字典序）可能让
	// 陈旧备份胜出——那账号在面板上表现为 token 失效。取不到 mtime 的文件排最前（先被覆盖）。
	sort.SliceStable(files, func(i, j int) bool {
		fi, erri := os.Stat(files[i])
		fj, errj := os.Stat(files[j])
		if erri != nil || errj != nil {
			return erri != nil && errj == nil
		}
		return fi.ModTime().Before(fj.ModTime())
	})
	// seenUID 重复 UID 检测：同一个号的**个人版与企业版 uid 完全相同**（企业版只是
	// type→ultimate + 多一个 enterpriseId，实测如此），两份凭证在池里只有一个槽位，
	// 后载入者胜出。这里打 WARN 并点出两个文件与各自 enterpriseId——否则用户只看到
	// 「我导了两个号，面板只有一个」。
	type dupUID struct{ file, eid string }
	seenUID := make(map[string]dupUID, len(files))
	var out []*Auth
	for _, f := range files {
		// 坏文件不再静默跳过：safefile.Read 在正文损坏时回退到 .bak（掉电留下的
		// 全 0 文件因此还能救回账号），两边都坏才放弃，并且说清是哪个文件、坏在哪
		// ——「账号凭空少了一个」比多打一行 ERROR 难查得多。
		raw, err := safefile.Read(f, func(b []byte) bool {
			_, perr := Parse(b)
			return perr == nil
		})
		if err != nil {
			log.Printf("ERROR: auth 文件 %s 无法读取: %v —— 该账号本次不在池里，修好文件或重新登录", f, err)
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			log.Printf("ERROR: auth 文件 %s 解析失败: %v —— 该账号本次不在池里", f, err)
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s 在 %s（enterpriseId=%q）与 %s（enterpriseId=%q）重复 —— "+
				"同一账号的个人版/企业版 uid 相同，池里只保留后载入的一份；不用的那份删掉",
				logfmt.Label(a.UID, a.Nickname), prev.file, prev.eid, f, a.EnterpriseID)
		}
		seenUID[a.UID] = dupUID{file: f, eid: a.EnterpriseID}
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
