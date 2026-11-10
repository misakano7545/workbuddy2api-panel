// channel.go 更新通道：代理、镜像前缀与失败归因（issue #198）。
//
// 现场：群晖 NAS 部署，api.github.com 通（查得到版本），但 release-assets.
// githubusercontent.com 被出站拦住 —— 连续 6 天 04:00 自动安装全失败在同一处，
// 面板只看到「下载更新文件失败: http2: timeout awaiting response headers」这种
// 原始错误，既不知道能配代理，也不知道自己走的是哪条通道。
//
// 本文件补三件事：
//  1. 显式出口（update.proxy）+ 是否沿用环境变量（update.proxy_from_env），
//     两者都随 Configure 热生效；
//  2. 资产下载镜像前缀（update.mirror）。**只作用于资产下载**：Release 元数据
//     （tag/digest/size）仍然直连 api.github.com，落盘后照旧校验 sha256 ——
//     镜像只能搬运字节，不能修改「应该是什么」。
//  3. 失败归因：dns / connect / tls / header-timeout / body-stall / checksum /
//     http，并给出对应建议动作，写进 status.last_error_kind。
package updater

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 失败类目（status.last_error_kind）。取值稳定，面板与文档按它给建议动作。
const (
	kindDNS          = "dns"
	kindConnect      = "connect"
	kindTLS          = "tls"
	kindHeaderTime   = "header-timeout"
	kindBodyStall    = "body-stall"
	kindChecksum     = "checksum"
	kindHTTP         = "http"
	kindProxyConfig  = "proxy-config"
	kindMirrorConfig = "mirror-config"
	kindUnknown      = "unknown"
)

// channelProxy 生效的出口设置（快照）。
type channelProxy struct {
	// URL 显式代理；空 = 未显式设置。
	URL string
	// FromEnv 是否允许读环境变量。
	FromEnv bool
	// Invalid 显式代理配错（非 http/https/socks5 或 URL 不合法）：已在 Configure
	// 里被拒，运行期按「不使用代理」处理，并在面板上把原因说清楚。
	Invalid string
}

// settingsSnapshot 复制一份当前设置（锁内取，避免读到半更新的组合）。
func (m *Manager) settingsSnapshot() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

// mirrorPrefix 规范化镜像前缀：空串 = 直连；否则保证以 "/" 结尾。
func mirrorPrefix(mirror string) string {
	mirror = strings.TrimSpace(mirror)
	if mirror == "" {
		return ""
	}
	if !strings.HasSuffix(mirror, "/") {
		mirror += "/"
	}
	return mirror
}

// validateProxy 校验显式代理：空 = 合法（不设置）；只接受 http/https/socks5。
func validateProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("代理地址无法解析: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("代理协议 %q 不受支持（只接受 http / https / socks5）", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("代理地址缺少主机名")
	}
	return nil
}

// validateMirror 校验镜像前缀：空 = 合法；必须是 http(s)://host/ 形式。
func validateMirror(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("镜像前缀无法解析: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("镜像前缀必须是 http(s):// 地址，当前 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("镜像前缀缺少主机名")
	}
	return nil
}

// transport 按当前设置构造 Transport。
//
// 与 http.DefaultTransport 的关系：克隆它（保留连接池/超时/H2 等既有行为），
// 只替换 Proxy 字段。三条线索的优先级：
//  1. 显式 proxy 配了就只用它；
//  2. 没配 proxy 且 proxy_from_env=false → 显式 direct（Proxy=nil）；
//  3. 其余情况保持 ProxyFromEnvironment（与 issue #198 描述的既有行为一致）。
func (m *Manager) transport() *http.Transport {
	s := m.settingsSnapshot()
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	tr := base.Clone()
	proxy := strings.TrimSpace(s.Proxy)
	switch {
	case proxy != "" && validateProxy(proxy) == nil:
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	case !s.ProxyFromEnv:
		// 显式忽略环境变量：容器里被别处塞了 HTTP_PROXY、但更新要走直连时用。
		tr.Proxy = nil
	}
	return tr
}

// httpClient 返回一个按当前设置构造请求通道的客户端。
//
// 若 Manager.client.Transport 是测试注入的 RoundTripper（非 *http.Transport），
// 原样沿用、不叠加代理——夹具要能完全掌控流量；生产路径一律按设置重建 Transport，
// 这样 Configure 之后下一个请求就用新出口（热生效）。
func (m *Manager) httpClient(timeout time.Duration) *http.Client {
	m.mu.RLock()
	base := m.client
	m.mu.RUnlock()
	if base == nil {
		return &http.Client{Timeout: timeout, Transport: m.transport()}
	}
	c := *base
	c.Timeout = timeout
	if rt := base.Transport; rt != nil {
		if _, ok := rt.(*http.Transport); !ok {
			c.Transport = rt // 测试注入：完全交给夹具
			return &c
		}
	}
	c.Transport = m.transport()
	return &c
}

// channelLabels 当前生效通道的人类可读描述（面板展示）。
func (m *Manager) channelLabels() (channel, proxyHost string) {
	s := m.settingsSnapshot()
	proxy := strings.TrimSpace(s.Proxy)
	mirror := mirrorPrefix(s.Mirror)
	switch {
	case proxy != "" && validateProxy(proxy) == nil:
		if u, err := url.Parse(proxy); err == nil {
			proxyHost = u.Host
		}
	case !s.ProxyFromEnv:
		proxyHost = ""
	default:
		// 走环境变量：这里如实说「按环境变量」，不猜测具体是哪条 —— 猜错比不写更坑。
		proxyHost = ""
	}
	switch {
	case mirror != "" && proxyHost != "":
		channel = "mirror+proxy"
	case mirror != "":
		channel = "mirror"
	case proxyHost != "":
		channel = "proxy"
	case !s.ProxyFromEnv:
		channel = "direct"
	default:
		channel = "direct(env)"
	}
	return channel, proxyHost
}

// assetFetchURL 资产的实际下载地址：配了镜像就套前缀，否则用原始地址。
//
// 镜像只换「去哪儿拿字节」，不换「应该拿到什么」——尺寸与 sha256 仍然来自
// 直连 api.github.com 的 Release 元数据（见 Install）。
func assetFetchURL(original, mirror string) string {
	prefix := mirrorPrefix(mirror)
	if prefix == "" {
		return original
	}
	return prefix + original
}

// mirrorHost 镜像前缀的主机名（用于信任判据：镜像域可以出现在初始 URL 上）。
func mirrorHost(mirror string) string {
	prefix := mirrorPrefix(mirror)
	if prefix == "" {
		return ""
	}
	u, err := url.Parse(prefix)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

// hostTrusted 判断地址是否可信。
//
// initial=true：允许 GitHub 官方域，以及**已配置镜像**的主机（只在这一处放宽，
// 且只在初始 URL 上放宽）。
// initial=false（重定向目标）：GitHub 官方域或该镜像自身主机；镜像不能把客户端
// 导向任意域，否则「信任镜像」就等于「信任它能把你带到任何地方」。
func (m *Manager) hostTrusted(u *url.URL, initial bool) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	if isTrustedGitHubURL(u, initial) {
		return true
	}
	if !initial {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return false
	}
	mh := mirrorHost(m.settingsSnapshot().Mirror)
	return mh != "" && host == mh
}

// redirectTrusted 重定向目标判据：GitHub 官方域或镜像主机（镜像自身端口也允许，
// 有些镜像走非标端口；初始地址仍要求 https 且无端口）。
func (m *Manager) redirectTrusted(u *url.URL) bool {
	if u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return false
	}
	if isTrustedGitHubURL(u, false) {
		return true
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	mh := mirrorHost(m.settingsSnapshot().Mirror)
	if mh != "" && host == mh {
		return true
	}
	return false
}

// ---------------------------------------------------------------- 失败归因

// kindOf 把一次网络错误归到一个类目。判据只用标准库类型与错误串，不做猜测：
//   - *net.DNSError → dns
//   - 证书/握手类 → tls
//   - 等响应头超时 → header-timeout（Go 的 HTTP/2 与 HTTP/1.1 都这么报）
//   - 其余超时/拨号失败 → connect
//   - 落到这里其他情况 → connect（有连接错误时它就是原因；纯未知才 unknown）
func kindOf(err error) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return kindDNS
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return kindTLS
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return kindTLS
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return kindTLS
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout awaiting response headers"),
		strings.Contains(msg, "awaiting response headers"):
		return kindHeaderTime
	case strings.Contains(msg, "tls:"),
		strings.Contains(msg, "x509:"),
		strings.Contains(msg, "certificate"),
		strings.Contains(msg, "handshake"):
		return kindTLS
	case strings.Contains(msg, "no such host"):
		return kindDNS
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "no route to host"),
		strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "dial tcp"):
		return kindConnect
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return kindConnect
	}
	return kindUnknown
}

// adviceFor 给出该失败类目对应的建议动作（写进日志，面板显示 kind + 原文）。
// 文案对齐 issue #198 的诉求：运维看日志就知道下一步做什么。
func adviceFor(kind string) string {
	switch kind {
	case kindDNS:
		return "域名解析失败：检查容器 DNS（可用 dns_servers）与出站策略"
	case kindConnect:
		return "TCP 连不上：出站被拦或网络不可达；可配置 update.proxy 或 update.mirror 绕行"
	case kindTLS:
		return "TLS 握手/证书失败：常见于中间设备劫持或系统 CA 过期；先试 update.mirror"
	case kindHeaderTime:
		return "等响应头超时：出站到 release-assets 很可能被拦（api.github.com 往往仍通）；请配置 update.proxy 或 update.mirror"
	case kindBodyStall:
		return "下载中途停滞：链路抖动或镜像限速；可换镜像或配置 update.proxy"
	case kindChecksum:
		return "摘要/尺寸不符：下载内容被中间层改写；请换回直连或换可信镜像（不要放宽校验）"
	case kindHTTP:
		return "对方返回非 200：镜像不可用或路径不对；检查 update.mirror 前缀"
	case kindProxyConfig:
		return "update.proxy 配置不合法；支持 http:// https:// socks5://"
	case kindMirrorConfig:
		return "update.mirror 配置不合法；需要 http(s)://host/ 形式的前缀"
	}
	return ""
}

// classifyInto 把错误归因写进 status（last_error_kind），返回带类目的错误文本。
func (m *Manager) classifyInto(err error) error {
	if err == nil {
		return nil
	}
	kind := kindOf(err)
	if kind == "" {
		kind = kindUnknown
	}
	m.mu.Lock()
	m.status.LastErrorKind = kind
	m.mu.Unlock()
	if advice := adviceFor(kind); advice != "" {
		return fmt.Errorf("%w（[%s] %s）", err, kind, advice)
	}
	return err
}

// ProbeResult 一次连通性探测结果（面板「测试连通性」按钮）。
type ProbeResult struct {
	Target  string `json:"target"`
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Elapsed int64  `json:"elapsed_ms"`
	Kind    string `json:"kind,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Probe 分别探 Release 元数据（api.github.com）与资产下载通道。
//
// 只发 HEAD / 极短 GET：目的是回答「这条路通不通」，不下载任何东西。
// 资产侧探的是**镜像前缀本身**（配了镜像时）或 github.com —— release-assets
// 的真实地址需要 Release 元数据才有，那属于 Check 的职责，这里不重复。
func (m *Manager) Probe(ctx context.Context) (meta, asset ProbeResult) {
	// Accept 必须分腿给：元数据要 GitHub 的 JSON 媒体类型，资产腿探的是
	// 「一个普通下载地址能不能连通」——带上 vnd.github+json 去问 github.com 的
	// HTML 首页会被回 406（实测），于是「链路其实通」被判成异常，误导运维。
	meta = m.probeOne(ctx, "Release 元数据",
		strings.TrimRight(m.apiBaseURL, "/")+"/repos/"+m.repository, "application/vnd.github+json")
	s := m.settingsSnapshot()
	assetTarget := "资产下载"
	assetURL := "https://github.com"
	if prefix := mirrorPrefix(s.Mirror); prefix != "" {
		assetURL = strings.TrimRight(prefix, "/")
		assetTarget = "资产下载（镜像）"
	}
	asset = m.probeOne(ctx, assetTarget, assetURL, "*/*")
	return meta, asset
}

func (m *Manager) probeOne(ctx context.Context, target, rawURL, accept string) ProbeResult {
	res := ProbeResult{Target: target, URL: rawURL}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, rawURL, nil)
	if err != nil {
		res.Kind, res.Error = kindUnknown, err.Error()
		return res
	}
	req.Header.Set("User-Agent", "workbuddy2api-panel-updater")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	start := time.Now()
	resp, err := m.httpClient(15 * time.Second).Do(req)
	res.Elapsed = time.Since(start).Milliseconds()
	if err != nil {
		res.Kind = kindOf(err)
		// 探活阶段的对端直接关连接（常见于不存在的域名/黑洞地址）报的是裸 EOF，
		// 归因表里没有它。这里就地补一条：还没拿到任何响应就结束，语义就是连不上。
		// 只在探测路径补，不动 kindOf——下载中途的 EOF 属于 body-stall，别被抢。
		if res.Kind == "" || res.Kind == kindUnknown {
			if errors.Is(err, io.EOF) {
				res.Kind = kindConnect
			} else {
				res.Kind = kindUnknown
			}
		}
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	// 200/401/403/404 都说明「网络通」：401/403 是匿名限流或需要 token，不是通道问题。
	res.OK = resp.StatusCode < 500
	if !res.OK {
		res.Kind = kindHTTP
		res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return res
}
