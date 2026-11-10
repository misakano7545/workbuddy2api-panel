package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 配置校验

func TestValidateProxy(t *testing.T) {
	for _, tc := range []struct {
		in    string
		ok    bool
		label string
	}{
		{"", true, "空 = 不设置"},
		{"http://127.0.0.1:7890", true, "http"},
		{"https://proxy.example:8443", true, "https"},
		{"socks5://127.0.0.1:1080", true, "socks5"},
		{"socks5h://127.0.0.1:1080", true, "socks5h（DNS 走代理解析）"},
		{"  socks5://127.0.0.1:1080  ", true, "前后空白可容忍"},
		{"127.0.0.1:7890", false, "缺协议头"},
		{"ftp://proxy:21", false, "不支持的协议"},
		{"http://", false, "缺主机名"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			err := validateProxy(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("validateProxy(%q) err=%v want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}

func TestValidateMirror(t *testing.T) {
	for _, tc := range []struct {
		in    string
		ok    bool
		label string
	}{
		{"", true, "空 = 直连"},
		{"https://ghfast.top/", true, "带尾斜杠"},
		{"https://ghfast.top", true, "不带尾斜杠（自动补齐）"},
		{"http://mirror.internal:8080/proxy/", true, "内网 http 镜像"},
		{"ghfast.top", false, "缺协议"},
		{"ftp://mirror/", false, "不支持的协议"},
		{"https://", false, "缺主机名"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if err := validateMirror(tc.in); (err == nil) != tc.ok {
				t.Fatalf("validateMirror(%q) err=%v want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}

// 镜像前缀只做字符串拼接，且保证有一个斜杠分界（否则会拼出
// https://ghfast.tophttps://github.com/... 这种双协议）。
func TestAssetFetchURL(t *testing.T) {
	const orig = "https://github.com/o/r/releases/download/v1/f.zip"
	for _, tc := range []struct{ mirror, want string }{
		{"", orig},
		{"https://ghfast.top/", "https://ghfast.top/" + orig},
		{"https://ghfast.top", "https://ghfast.top/" + orig},
		{"http://m.local:8080/p/", "http://m.local:8080/p/" + orig},
	} {
		if got := assetFetchURL(orig, tc.mirror); got != tc.want {
			t.Fatalf("mirror=%q got %q want %q", tc.mirror, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------- 信任判据

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 镜像只能出现在**初始**地址上；重定向目标仍只认 GitHub 官方域或该镜像自身主机。
//
// 变异判据：把 hostTrusted 里的镜像分支去掉，镜像模式下 Install 会直接报
// 「下载地址不受信任」；把 redirectTrusted 的镜像分支去掉，镜像 302 回源会失败。
func TestMirrorTrustBoundary(t *testing.T) {
	m := New("dev", "o/r", "", Settings{Mirror: "https://ghfast.top"})
	for _, tc := range []struct {
		raw     string
		initial bool
		want    bool
		label   string
	}{
		{"https://github.com/o/r/releases/download/v1/f.zip", true, true, "初始：GitHub 官方域"},
		{"https://ghfast.top/https://github.com/o/r/x.zip", true, true, "初始：已配置镜像"},
		{"https://ghfast.top/x.zip", true, true, "初始：镜像简写"},
		{"https://evil.example/x.zip", true, false, "初始：陌生域"},
		{"http://ghfast.top/x.zip", true, false, "初始：http 不接受（镜像配的是 https）"},
		{"https://release-assets.githubusercontent.com/x", false, true, "重定向：GitHub 资产域"},
		{"https://ghfast.top/x", false, true, "重定向：镜像自身"},
		{"https://evil.example/x", false, false, "重定向：陌生域"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			u := mustURL(t, tc.raw)
			var got bool
			if tc.initial {
				got = m.hostTrusted(u, true)
			} else {
				got = m.redirectTrusted(u)
			}
			if got != tc.want {
				t.Fatalf("%s: got %v want %v", tc.raw, got, tc.want)
			}
		})
	}
	// 没配镜像时，陌生域一律不许（包括「镜像」这个概念本身不存在）。
	plain := New("dev", "o/r", "", Settings{})
	if plain.hostTrusted(mustURL(t, "https://ghfast.top/x"), true) {
		t.Fatal("未配置镜像时不得信任任何第三方域")
	}
	if plain.redirectTrusted(mustURL(t, "https://ghfast.top/x")) {
		t.Fatal("未配置镜像时重定向也不得信任第三方域")
	}
}

// ---------------------------------------------------------------- 出口

func TestTransportHonoursSettings(t *testing.T) {
	// 1) 显式代理：Proxy 必须指向它。
	m := New("dev", "o/r", "", Settings{Proxy: "socks5://127.0.0.1:1080", ProxyFromEnv: true})
	tr := m.transport()
	if tr.Proxy == nil {
		t.Fatal("显式 proxy 未被应用")
	}
	got, err := tr.Proxy(&http.Request{URL: mustURL(t, "https://api.github.com/")})
	if err != nil || got == nil || got.Host != "127.0.0.1:1080" {
		t.Fatalf("proxy=%v err=%v want 127.0.0.1:1080", got, err)
	}

	// 2) 关闭环境变量且不配代理：Proxy=nil（直连，不受宿主 HTTP_PROXY 影响）。
	t.Setenv("HTTPS_PROXY", "http://should-be-ignored:3128")
	m = New("dev", "o/r", "", Settings{ProxyFromEnv: false})
	tr = m.transport()
	if tr.Proxy != nil {
		if got, _ := tr.Proxy(&http.Request{URL: mustURL(t, "https://api.github.com/")}); got != nil {
			t.Fatalf("proxy_from_env=false 仍走了代理 %v", got)
		}
	}
}

// 热生效：Configure 之后下一次请求就用新出口（不必重启进程）。
func TestConfigureSwitchesChannelHot(t *testing.T) {
	m := New("dev", "o/r", "", Settings{AutoEnabled: true, AutoTime: "04:00", ProxyFromEnv: true})
	before := m.Status().Channel
	if before != "direct(env)" {
		t.Fatalf("初始通道 = %q want direct(env)", before)
	}
	m.Configure(Settings{AutoEnabled: true, AutoTime: "04:00", Mirror: "https://ghfast.top/"})
	if got := m.Status(); got.Channel != "mirror" || got.Mirror != "https://ghfast.top/" {
		t.Fatalf("切镜像后 status=%+v", got)
	}
	m.Configure(Settings{AutoEnabled: true, AutoTime: "04:00", Proxy: "socks5://127.0.0.1:1080", Mirror: "https://ghfast.top/"})
	if got := m.Status(); got.Channel != "mirror+proxy" || got.ProxyHost != "127.0.0.1:1080" {
		t.Fatalf("镜像+代理 status=%+v", got)
	}
	// 非法值不覆盖上一份：面板里填错一个字符不该把已生效的通道打掉。
	m.Configure(Settings{AutoEnabled: true, AutoTime: "04:00", Mirror: "ghfast.top"})
	if got := m.Status(); got.Mirror != "https://ghfast.top/" {
		t.Fatalf("非法镜像覆盖了生效值：%+v", got)
	}
}

// ---------------------------------------------------------------- 失败归因

func TestKindOf(t *testing.T) {
	tlsErr := &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}
	for _, tc := range []struct {
		err   error
		kind  string
		label string
	}{
		{&net.DNSError{Err: "no such host", Name: "github.com"}, kindDNS, "DNS"},
		{tlsErr, kindTLS, "证书校验"},
		{x509.UnknownAuthorityError{}, kindTLS, "未知 CA"},
		{x509.HostnameError{Host: "x", Certificate: &x509.Certificate{}}, kindTLS, "主机名不符"},
		{errors.New(`Get "https://x": http2: timeout awaiting response headers`), kindHeaderTime, "等响应头超时（issue #198 现场原文）"},
		{errors.New("dial tcp 20.205.243.166:443: i/o timeout"), kindConnect, "拨号超时（issue #198 现场原文）"},
		{errors.New("dial tcp: lookup api.github.com on 127.0.0.53:53: no such host"), kindDNS, "解析失败"},
		{errors.New("something odd"), kindUnknown, "未归类"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if got := kindOf(tc.err); got != tc.kind {
				t.Fatalf("kindOf(%v) = %q want %q", tc.err, got, tc.kind)
			}
		})
	}
	if kindOf(nil) != "" {
		t.Fatal("nil 应返回空类目")
	}
	// 每个类目都必须有建议动作：面板把它直接展示给运维，空文案等于没提示。
	for _, k := range []string{kindDNS, kindConnect, kindTLS, kindHeaderTime, kindBodyStall,
		kindChecksum, kindHTTP, kindProxyConfig, kindMirrorConfig} {
		if adviceFor(k) == "" {
			t.Fatalf("类目 %q 缺建议文案", k)
		}
	}
}

// 下载失败要把类目写进 status，并把建议动作拼进错误文本（日志里一眼能看到下一步）。
//
// 直接调 fetchAsset 而不是 Install：Install 在下载之后会 exec 自己去做二进制替换，
// 在测试进程里既慢又有副作用；通道与归因的断言全部发生在下载这一段之内。
func TestDownloadFailureRecordsKind(t *testing.T) {
	m := New("2026.11.10.1200", "o/r", "", Settings{})
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 20.205.243.166:443: i/o timeout")
	})
	asset := &releaseAsset{
		Name:               "x.zip",
		BrowserDownloadURL: "https://github.com/o/r/releases/download/v1/f.zip",
		Size:               10,
		Digest:             "sha256:" + strings.Repeat("0", 64),
	}
	_, _, err := m.fetchAsset(context.Background(), asset)
	if err == nil {
		t.Fatal("下载失败应返回错误")
	}
	if !strings.Contains(err.Error(), "["+kindConnect+"]") {
		t.Fatalf("错误文本应带类目与建议：%v", err)
	}
	if kind := m.Status().LastErrorKind; kind != kindConnect {
		t.Fatalf("status.last_error_kind = %q want %q", kind, kindConnect)
	}
}

// ---------------------------------------------------------------- 探测

func TestProbeReportsBothLegs(t *testing.T) {
	m := New("dev", "o/r", "", Settings{Mirror: "https://mirror.example/"})
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Host, "api.github.com"):
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]")),
				Header: make(http.Header)}, nil
		case r.URL.Host == "mirror.example":
			return nil, errors.New("dial tcp 1.2.3.4:443: i/o timeout")
		}
		return nil, errors.New("unexpected host " + r.URL.Host)
	})
	meta, asset := m.Probe(context.Background())
	if !meta.OK || meta.Status != 200 {
		t.Fatalf("元数据腿应通：%+v", meta)
	}
	if asset.OK || asset.Kind != kindConnect {
		t.Fatalf("镜像腿应失败并归为 connect：%+v", asset)
	}
	if asset.URL != "https://mirror.example" {
		t.Fatalf("资产腿应探镜像前缀，实际 %q", asset.URL)
	}
}

// ---------------------------------------------------------------- 镜像安装（端到端）

// 配了镜像时：资产只走镜像，且原 URL 作为路径前缀拼在后面。
//
// 变异判据：把 fetchAsset 里的 assetFetchURL 换回 BrowserDownloadURL，本用例会在
// 「资产请求打到了 github.com」上红。
func TestFetchAssetThroughMirror(t *testing.T) {
	archive := buildUpdateArchive(t)
	sum := sha256.Sum256(archive)
	mirrorHost := "mirror.example"
	m := New("2026.11.10.1200", "o/r", "", Settings{Mirror: "https://" + mirrorHost + "/"})
	var assetHits int
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != mirrorHost {
			t.Errorf("资产必须走镜像，实际请求 %q", r.URL.String())
		}
		assetHits++
		if !strings.HasPrefix(r.URL.Path, "/https://github.com/o/r/releases/download/v1/") {
			t.Errorf("镜像收到的路径不是原始 URL 前缀：%q", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(archive)),
			ContentLength: int64(len(archive)), Header: make(http.Header)}, nil
	})
	asset := &releaseAsset{
		Name:               "pkg.zip",
		BrowserDownloadURL: "https://github.com/o/r/releases/download/v1/pkg.zip",
		Size:               int64(len(archive)),
		Digest:             "sha256:" + hex.EncodeToString(sum[:]),
	}
	path, cleanup, err := m.fetchAsset(context.Background(), asset)
	defer cleanup()
	if err != nil {
		t.Fatalf("镜像下载失败: %v", err)
	}
	if assetHits != 1 {
		t.Fatalf("镜像命中 %d 次 want 1", assetHits)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, archive) {
		t.Fatalf("落盘内容与原包不一致（err=%v, len=%d/%d）", err, len(got), len(archive))
	}
}

// 镜像返回被改写的内容 → 摘要不符，必须中止（镜像不能投毒）。
func TestFetchAssetRejectsTamperedBody(t *testing.T) {
	archive := buildUpdateArchive(t)
	tampered := append([]byte{}, archive...)
	tampered[len(tampered)-1] ^= 0xFF // 同长度、不同内容：尺寸检查过不了，只能靠摘要拦
	sum := sha256.Sum256(archive)
	m := New("2026.11.10.1200", "o/r", "", Settings{Mirror: "https://mirror.example/"})
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(tampered)),
			ContentLength: int64(len(tampered)), Header: make(http.Header)}, nil
	})
	asset := &releaseAsset{
		Name:               "pkg.zip",
		BrowserDownloadURL: "https://github.com/o/r/releases/download/v1/pkg.zip",
		Size:               int64(len(tampered)),
		Digest:             "sha256:" + hex.EncodeToString(sum[:]),
	}
	_, cleanup, err := m.fetchAsset(context.Background(), asset)
	defer cleanup()
	if err == nil {
		t.Fatal("被改写的下载内容必须让安装失败")
	}
	if !strings.Contains(err.Error(), kindChecksum) {
		t.Fatalf("错误应指向摘要不符，实际：%v", err)
	}
	if kind := m.Status().LastErrorKind; kind != kindChecksum {
		t.Fatalf("status.last_error_kind = %q want %q", kind, kindChecksum)
	}
}

// buildUpdateArchive 造一个可解包的最小更新包（含当前平台二进制名）。
func buildUpdateArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	bin := "wb2api"
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	w, err := zw.Create(bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("#!/bin/sh\necho fake\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jsonRespForTest 把对象编码成 200 响应（Transport 桩用）。
func jsonRespForTest(t *testing.T, v any) *http.Response {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)), Header: make(http.Header)}
}
