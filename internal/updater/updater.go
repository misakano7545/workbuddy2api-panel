// Package updater checks timestamped GitHub releases and installs native binaries.
package updater

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxArchiveSize = 300 << 20
	maxBinarySize  = 200 << 20
	selfUpdateArg  = "--wb2api-self-update"
	cleanupArg     = "--wb2api-cleanup"
)

// 更新流程的时间参数集中在这里，测试可注入短值。
var (
	// exeReleaseBudget 删「刚跑完的 exe」的重试预算（见 removeFileRetrying）。
	// 实测 Windows 上 Access denied 持续 50~150ms（本机 6 轮里回滚/清理两条路径
	// 各命中一次，首次必败、一次 20ms 重试即成功），2s 留足余量；真失败时也只
	// 多花这 2s，不会把更新流程拖住。
	exeReleaseBudget = 2 * time.Second
	// downloadStallWindow 下载「N 秒无新字节」卡死守卫窗口。
	downloadStallWindow = 30 * time.Second
	// installWatchdog Install「成功」后，等 helper 完成替换的看门狗期限。
	installWatchdog = 90 * time.Second
	// childGraceWindow 新版本进程拉起后的健康观察窗口，活过才删回滚备份。
	childGraceWindow = 8 * time.Second
	// staleTempAge 启动时清扫更新残留临时文件的修改时间阈值；helper 的整个
	// 生命周期（下载+替换+观察窗口）远短于该值，不会误删在途更新。
	staleTempAge = 6 * time.Hour
)

// childGrace 生效的健康观察窗口。
//
// helper 是**独立进程**（exec 自己），所以测试注入只能经环境变量传下去。为了不让
// 运维环境里一个随手写的变量把观察窗口缩到无害区（那就等于关掉了「启动即崩就回滚」
// 这层保护），注入只在夹具模式（WB2API_UPDATE_FIXTURE 非空，仅集成测试会设）下生效。
func childGrace() time.Duration {
	if os.Getenv("WB2API_UPDATE_FIXTURE") == "" {
		return childGraceWindow
	}
	if v := strings.TrimSpace(os.Getenv("WB2API_UPDATE_GRACE_MS")); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 && ms <= 60000 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return childGraceWindow
}

// Settings controls the optional daily automatic install.
type Settings struct {
	AutoEnabled bool
	AutoTime    string
}

// Status is safe to expose through the authenticated panel API.
type Status struct {
	CurrentVersion  string `json:"current_version"`
	CurrentKnown    bool   `json:"current_known"` // Whether the current version can be compared with Releases.
	LatestVersion   string `json:"latest_version"`
	ReleaseURL      string `json:"release_url"`
	ReleaseNotes    string `json:"release_notes"`
	CheckedAt       string `json:"checked_at"`
	LastError       string `json:"last_error"`
	UpdateAvailable bool   `json:"update_available"`
	CanInstall      bool   `json:"can_install"`
	Installing      bool   `json:"installing"`
	AutoEnabled     bool   `json:"auto_enabled"`
	AutoTime        string `json:"auto_time"`
}

type release struct {
	TagName     string         `json:"tag_name"`
	HTMLURL     string         `json:"html_url"`
	Body        string         `json:"body"`
	Draft       bool           `json:"draft"`
	Prerelease  bool           `json:"prerelease"`
	PublishedAt string         `json:"published_at"`
	Assets      []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
}

type parsedVersion struct {
	text    string
	ordinal int64
}

// Manager owns update discovery and the daily schedule.
type Manager struct {
	version    string
	repository string
	executable string
	configPath string
	apiBaseURL string
	client     *http.Client
	now        func() time.Time

	mu          sync.RWMutex
	checkMu     sync.Mutex
	settings    Settings
	status      Status
	release     *release
	lastCheck   time.Time
	lastAutoDay string
	restarting  func()
}

// New creates an updater for a standalone process. An empty repository falls back
// to the source repository embedded by the build workflow.
func New(version, repository, configPath string, settings Settings) *Manager {
	executable, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	if repository == "" {
		repository = "misakano7545/workbuddy2api-panel"
	}
	if settings.AutoTime == "" {
		settings.AutoTime = "04:00"
	}
	m := &Manager{
		version:    strings.TrimPrefix(version, "v"),
		repository: repository,
		executable: executable,
		configPath: configPath,
		apiBaseURL: "https://api.github.com",
		client:     &http.Client{Timeout: 30 * time.Second},
		now:        time.Now,
		settings:   settings,
		status: Status{
			CurrentVersion: version,
			AutoEnabled:    settings.AutoEnabled,
			AutoTime:       settings.AutoTime,
		},
	}
	_, m.status.CurrentKnown = parseVersion(m.version)
	m.status.CanInstall = m.canInstallLocked()
	if executable != "" {
		CleanupStaleTempFiles(filepath.Dir(executable), staleTempAge)
	}
	return m
}

// CleanupStaleTempFiles 清扫程序目录里上一次更新失败残留的临时文件
// （.wb2api-stage-* / .wb2api-updater-* / .wb2api-release-*.zip），只清修改时间
// 超过阈值的（在途更新的整个生命周期远短于阈值）；回滚备份 *.old 一律不动——
// 那是失败现场。helper 侧多个失败分支（pid 无效、参数损坏、旧进程迟迟不退）此前
// 会把 stage/helper 永久留在磁盘上，这里在下次启动时兜底回收。
func CleanupStaleTempFiles(dir string, olderThan time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, ".wb2api-stage-") &&
			!strings.HasPrefix(name, ".wb2api-updater-") &&
			!strings.HasPrefix(name, ".wb2api-release-") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			log.Printf("[updater] 已清扫更新残留文件 %s（修改于 %s）", name, info.ModTime().Format(time.RFC3339))
		}
	}
}

// SetRestart installs the callback used after an update helper is ready.
func (m *Manager) SetRestart(fn func()) {
	m.mu.Lock()
	m.restarting = fn
	m.mu.Unlock()
}

// Configure applies settings without restarting the service.
func (m *Manager) Configure(settings Settings) {
	if settings.AutoTime == "" {
		settings.AutoTime = "04:00"
	}
	m.mu.Lock()
	m.settings = settings
	m.status.AutoEnabled = settings.AutoEnabled
	m.status.AutoTime = settings.AutoTime
	m.mu.Unlock()
}

// Status returns a consistent snapshot for the panel.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Check fetches timestamped, non-prerelease Releases and selects the newest one.
func (m *Manager) Check(ctx context.Context) error {
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	releases, err := m.fetchReleases(ctx)
	if err == nil {
		var newest *release
		var newestVersion parsedVersion
		for i := range releases {
			r := &releases[i]
			if r.Draft || r.Prerelease {
				continue
			}
			v, ok := parseVersion(r.TagName)
			if !ok {
				continue
			}
			if newest == nil || v.ordinal > newestVersion.ordinal {
				newest, newestVersion = r, v
			}
		}
		if newest == nil {
			err = errors.New("没有找到时间版本格式的正式 Release")
		} else {
			releaseURL := ""
			if releaseLink, parseErr := url.Parse(newest.HTMLURL); parseErr == nil && isTrustedGitHubURL(releaseLink, true) {
				releaseURL = newest.HTMLURL
			}
			m.mu.Lock()
			m.release = newest
			m.status.LatestVersion = newestVersion.text
			m.status.ReleaseURL = releaseURL
			m.status.ReleaseNotes = newest.Body
			current, currentOK := parseVersion(m.version)
			// An unknown build may be newer than the Release; do not advertise a downgrade.
			m.status.UpdateAvailable = currentOK && newestVersion.ordinal > current.ordinal
			m.status.CanInstall = m.canInstallLocked()
			m.status.LastError = ""
			m.status.CheckedAt = m.now().Format(time.RFC3339)
			m.lastCheck = m.now()
			m.mu.Unlock()
			return nil
		}
	}

	m.mu.Lock()
	m.status.LastError = err.Error()
	m.status.CheckedAt = m.now().Format(time.RFC3339)
	m.lastCheck = m.now()
	m.mu.Unlock()
	return err
}

func (m *Manager) fetchReleases(ctx context.Context) ([]release, error) {
	var all []release
	for page := 1; page <= 5; page++ {
		url := fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", strings.TrimRight(m.apiBaseURL, "/"), m.repository, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "workbuddy2api-panel-updater")
		resp, err := m.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("查询 GitHub Release 失败: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, fmt.Errorf("查询 GitHub Release 失败: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var batch []release
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&batch)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("解析 GitHub Release 失败: %w", err)
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// Install downloads and stages the matching release asset. The caller should
// send its response before calling RestartAfter.
func (m *Manager) Install(ctx context.Context) (err error) {
	m.mu.Lock()
	if m.status.Installing {
		m.mu.Unlock()
		return errors.New("更新已在进行中")
	}
	if !m.canInstallLocked() {
		m.mu.Unlock()
		return errors.New("当前部署不支持原地更新，请从 Release 页面下载或更新容器镜像")
	}
	if m.restarting == nil {
		m.mu.Unlock()
		return errors.New("更新重启回调未配置")
	}
	m.status.Installing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if err != nil {
			m.status.Installing = false
			m.status.LastError = err.Error()
		}
		m.mu.Unlock()
	}()

	if err := m.Check(ctx); err != nil {
		return err
	}
	m.mu.RLock()
	r := m.release
	m.mu.RUnlock()
	if r == nil || !m.Status().UpdateAvailable {
		return errors.New("当前已是最新版本")
	}
	version, ok := parseVersion(r.TagName)
	if !ok {
		return errors.New("Release 版本格式不受支持")
	}
	assetName := fmt.Sprintf("wb2api-panel-%s-%s-%s.zip", version.text, runtime.GOOS, runtime.GOARCH)
	var asset *releaseAsset
	for i := range r.Assets {
		if r.Assets[i].Name == assetName {
			asset = &r.Assets[i]
			break
		}
	}
	if asset == nil {
		return fmt.Errorf("Release 缺少当前平台文件 %s", assetName)
	}
	assetURL, err := url.Parse(asset.BrowserDownloadURL)
	if err != nil || !isTrustedGitHubURL(assetURL, true) {
		return errors.New("Release 下载地址不是受信任的 GitHub HTTPS 地址")
	}
	if asset.Size <= 0 || asset.Size > maxArchiveSize {
		return fmt.Errorf("Release 文件大小异常: %d", asset.Size)
	}
	if !hasSHA256Digest(asset.Digest) {
		return errors.New("Release 未提供有效 SHA-256 摘要，已取消安装")
	}

	dir := filepath.Dir(m.executable)
	archiveFile, err := os.CreateTemp(dir, ".wb2api-release-*.zip")
	if err != nil {
		return fmt.Errorf("创建下载临时文件: %w", err)
	}
	archivePath := archiveFile.Name()
	defer os.Remove(archivePath)
	if err := m.downloadAsset(ctx, asset, archiveFile); err != nil {
		archiveFile.Close()
		return err
	}
	if err := archiveFile.Sync(); err != nil {
		archiveFile.Close()
		return fmt.Errorf("同步下载文件: %w", err)
	}
	if err := archiveFile.Close(); err != nil {
		return fmt.Errorf("关闭下载文件: %w", err)
	}
	if err := verifySHA256(archivePath, strings.TrimPrefix(asset.Digest, "sha256:")); err != nil {
		return err
	}

	expectedBinary := "wb2api"
	if runtime.GOOS == "windows" {
		expectedBinary += ".exe"
	}
	stageFile, err := os.CreateTemp(dir, ".wb2api-stage-*"+filepath.Ext(expectedBinary))
	if err != nil {
		return fmt.Errorf("创建更新暂存文件: %w", err)
	}
	stagePath := stageFile.Name()
	if err := stageFile.Close(); err != nil {
		os.Remove(stagePath)
		return err
	}
	if err := extractBinary(archivePath, stagePath, expectedBinary); err != nil {
		os.Remove(stagePath)
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(stagePath, 0o755); err != nil {
			os.Remove(stagePath)
			return fmt.Errorf("设置更新文件权限: %w", err)
		}
	}

	helperFile, err := os.CreateTemp(dir, ".wb2api-updater-*"+filepath.Ext(expectedBinary))
	if err != nil {
		os.Remove(stagePath)
		return fmt.Errorf("创建更新进程文件: %w", err)
	}
	helperPath := helperFile.Name()
	if err := helperFile.Close(); err != nil {
		os.Remove(helperPath)
		os.Remove(stagePath)
		return err
	}
	// Run the helper protocol from the installed version, not an unstarted release.
	if err := copyFile(m.executable, helperPath); err != nil {
		os.Remove(helperPath)
		os.Remove(stagePath)
		return fmt.Errorf("准备更新进程: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(helperPath, 0o700); err != nil {
			os.Remove(helperPath)
			os.Remove(stagePath)
			return fmt.Errorf("设置更新进程权限: %w", err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Remove(helperPath)
		os.Remove(stagePath)
		return err
	}
	originalArgs, err := json.Marshal(os.Args[1:])
	if err != nil {
		os.Remove(helperPath)
		os.Remove(stagePath)
		return err
	}
	if err := launchUpdateHelper(helperPath, selfUpdateArg, strconv.Itoa(os.Getpid()), stagePath, m.executable, cwd, string(originalArgs)); err != nil {
		os.Remove(helperPath)
		os.Remove(stagePath)
		return fmt.Errorf("启动更新进程: %w", err)
	}
	log.Printf("[updater] 已准备 v%s，服务即将重启", version.text)
	m.armInstallWatchdog()
	return nil
}

// armInstallWatchdog Install「成功」只代表 helper 进程被拉起来了，真正的换二进制
// 发生在 helper 子进程里。若 helper 中途失败（Windows 下目标文件被占用、磁盘满、
// 新版本起不来），父进程继续活着且 Installing 永久为 true，之后每次安装都被
// 「更新已在进行中」挡住，只能手动重启进程恢复。看门狗在期限内发现进程没有重启
// 就复位状态并留下原因，面板可重试而不是永久卡死。
func (m *Manager) armInstallWatchdog() {
	time.AfterFunc(installWatchdog, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.status.Installing {
			return
		}
		m.status.Installing = false
		m.status.LastError = fmt.Sprintf("更新进程在 %s 内未完成替换，若服务仍在运行请检查日志后重试", installWatchdog)
	})
}

func (m *Manager) downloadAsset(ctx context.Context, asset *releaseAsset, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "workbuddy2api-panel-updater")
	baseClient := m.client
	if baseClient == nil {
		baseClient = &http.Client{Timeout: 30 * time.Second}
	}
	client := *baseClient
	// 整体 Client.Timeout 覆盖「连接到读完响应体」全程：更新包上限 300MB，慢网下
	// 30s 必超时。下载改为无整体超时 + 两级守卫：响应头限时（Transport）与「N 秒
	// 无新字节」卡死守卫（stallGuard）——只要数据在推进就允许慢慢下。
	client.Timeout = 0
	client.Transport = withHeaderTimeout(client.Transport)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("下载更新文件重定向次数过多")
		}
		if !isTrustedGitHubURL(req.URL, false) {
			return errors.New("下载更新文件重定向到非 GitHub 官方地址")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载更新文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载更新文件失败: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxArchiveSize {
		return errors.New("下载文件超过大小限制")
	}
	guard := &stallGuard{r: resp.Body, closeFn: resp.Body.Close, window: downloadStallWindow}
	guard.timer = time.AfterFunc(downloadStallWindow, func() { _ = guard.closeFn() })
	defer guard.timer.Stop()
	n, err := io.Copy(dst, io.LimitReader(guard, maxArchiveSize+1))
	if err != nil {
		return fmt.Errorf("下载更新文件失败（中断或 %.0fs 无进展）: %w", downloadStallWindow.Seconds(), err)
	}
	if n > maxArchiveSize {
		return errors.New("下载文件超过大小限制")
	}
	if asset.Size > 0 && n != asset.Size {
		return fmt.Errorf("下载文件大小不符: got %d, want %d", n, asset.Size)
	}
	return nil
}

// withHeaderTimeout 给下载 transport 补一个响应头限时（无则克隆默认 transport）。
// m.client 的 Transport 可能是测试注入的 roundTripper，保持原样不叠加。
func withHeaderTimeout(rt http.RoundTripper) http.RoundTripper {
	var tr *http.Transport
	if t, ok := rt.(*http.Transport); ok {
		tr = t.Clone()
	} else if rt == nil {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		return rt
	}
	tr.ResponseHeaderTimeout = 30 * time.Second
	return tr
}

// stallGuard 下载卡死守卫：N 秒无新字节就关闭响应体。客户端的 Response.Body
// 没有 SetReadDeadline（那是服务端 ResponseController 的能力，HTTP/1.1 的阻塞
// Read 等在 transport 读循环的 channel 上，连接级 deadline 也打断不了它），
// 只能靠 Close 掐断——Close 会让阻塞中的 Read 立刻返回错误。每读到新字节重置
// 定时器，「慢但在动」不受影响，「彻底不动」在窗口内快速失败。
type stallGuard struct {
	r       io.Reader
	closeFn func() error
	window  time.Duration
	timer   *time.Timer
}

func (s *stallGuard) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 && s.timer != nil {
		s.timer.Reset(s.window)
	}
	return n, err
}

// HandleSelfUpdate handles the private helper invocation used by Install.
// It returns handled=false for normal server startup.
func HandleSelfUpdate(args []string) (handled bool, err error) {
	if len(args) >= 4 && args[1] == cleanupArg {
		pid, parseErr := strconv.Atoi(args[2])
		if parseErr != nil || pid <= 0 {
			return true, errors.New("清理进程参数中的 pid 无效")
		}
		if err := validateHelperPath(args[3], ".wb2api-"); err != nil {
			return true, err
		}
		if err := waitForProcessExit(pid, 60*time.Second); err != nil {
			return true, err
		}
		// 进程「已退出」≠ 映像句柄已释放：紧接着删它正在跑的 exe 会被拒（见
		// removeFileRetrying）。本清理进程存在的唯一目的就是删那个文件，失败即
		// 意味着 helper 永远残留，所以在这里重试到释放为止。
		return true, removeFileRetrying(args[3], exeReleaseBudget)
	}
	if len(args) < 7 || args[1] != selfUpdateArg {
		return false, nil
	}
	pid, err := strconv.Atoi(args[2])
	if err != nil || pid <= 0 {
		return true, errors.New("更新进程参数中的 pid 无效")
	}
	stagePath, targetPath, cwd, originalArgsJSON := args[3], args[4], args[5], args[6]
	if err := validateHelperPath(stagePath, ".wb2api-"); err != nil {
		return true, err
	}
	if err := validateHelperPath(targetPath, ""); err != nil {
		return true, err
	}
	// 失败路径不再残留 stage：成功替换后显式移除，其余任何退出路径由 defer 兜底。
	replaced := false
	defer func() {
		if !replaced {
			os.Remove(stagePath)
		}
	}()
	var originalArgs []string
	if err := json.Unmarshal([]byte(originalArgsJSON), &originalArgs); err != nil {
		return true, fmt.Errorf("解析原始启动参数: %w", err)
	}
	if err := waitForProcessExit(pid, 60*time.Second); err != nil {
		return true, err
	}
	if err := os.Chdir(cwd); err != nil {
		return true, fmt.Errorf("切换工作目录: %w", err)
	}
	helperPath, _ := os.Executable()
	cleanupHelper := func() {
		if runtime.GOOS == "windows" {
			launchCleanup(targetPath, helperPath, os.Getpid())
		} else {
			_ = os.Remove(helperPath)
		}
	}
	startTarget := func() (*exec.Cmd, error) {
		cmd := exec.Command(targetPath, originalArgs...)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		return cmd, cmd.Start()
	}
	replaceFailed := func(reason error) error {
		cmd, restartErr := startTarget() // 替换失败时 target 仍是旧二进制，把服务拉回来
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Release()
		}
		_ = os.Remove(stagePath)
		replaced = true
		cleanupHelper()
		return errors.Join(reason, restartErr)
	}
	restore, cleanup, err := replaceBinaryWithRollback(stagePath, targetPath)
	if err != nil {
		return true, replaceFailed(fmt.Errorf("替换程序文件: %w", err))
	}
	_ = os.Remove(stagePath)
	replaced = true
	cmd, err := startTarget()
	if err != nil {
		restartErr := restore()
		if restartErr == nil {
			if old, serr := startTarget(); serr == nil {
				_ = old.Process.Release()
			} else {
				restartErr = serr
			}
		}
		cleanupHelper()
		return true, errors.Join(fmt.Errorf("启动新版本: %w", err), restartErr)
	}
	// 拉起 ≠ 能跑：配置不兼容、端口被占、依赖缺失都会让新版本启动即退。给它一个
	// 短的观察窗口，窗口内退出就回滚旧版本（备份还在）；活过窗口才删备份与 helper。
	if werr := waitChildHealthy(cmd, childGrace()); werr != nil {
		if rerr := restore(); rerr != nil {
			log.Printf("WARN: [updater] 新版本健康检查未通过且回滚失败: %v", rerr)
		} else if old, serr := startTarget(); serr == nil {
			_ = old.Process.Release()
		} else {
			log.Printf("WARN: [updater] 回滚后重启旧版本失败: %v", serr)
		}
		cleanupHelper()
		return true, fmt.Errorf("新版本健康检查未通过: %w", werr)
	}
	_ = cmd.Process.Release()
	cleanup()
	cleanupHelper()
	return true, nil
}

// validateHelperPath 把私有子命令的文件参数收敛到「自己程序目录里的更新文件」：
// helper 与目标二进制同目录、临时文件必须是 .wb2api- 前缀。入口本身没有来源校验
// （能本机执行通常已等同失陷），但其余环节（域名白名单、SHA-256、zip 路径穿越）
// 都设了防，这里补上最后一块：把任意路径的删除/替换收敛为只能动自己的更新文件。
func validateHelperPath(path, namePrefix string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位更新进程自身: %w", err)
	}
	if filepath.Dir(path) != filepath.Dir(self) {
		return fmt.Errorf("更新文件参数不在程序目录内: %s", path)
	}
	if namePrefix != "" && !strings.HasPrefix(filepath.Base(path), namePrefix) {
		return fmt.Errorf("更新文件名需以 %s 开头: %s", namePrefix, filepath.Base(path))
	}
	return nil
}

func launchUpdateHelper(helperPath string, args ...string) error {
	cmd := exec.Command(helperPath, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	setHiddenWindow(cmd)
	return startChild(cmd)
}

func startChild(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func replaceBinary(stagePath, targetPath string) error {
	_, cleanup, err := replaceBinaryWithRollback(stagePath, targetPath)
	if err != nil {
		return err
	}
	cleanup()
	return nil
}

// removeFileRetrying 删一个刚被进程执行过的文件，Access denied 时退避重试。
//
// 为什么需要：Windows 上 WaitForSingleObject(WAIT_OBJECT_0) 只保证进程**终止**，
// 不保证它的映像 section 已从文件上解绑；紧接着 DeleteFile 会报 ERROR_ACCESS_DENIED。
// 本机实测（2026-10-29，updater 集成测试 6 轮）两条路径都命中：
//
//	回滚：新版本进程在观察窗口内退出 → remove(target) → Access denied，50~103ms 后重试成功
//	清理：清理进程等到 helper 退出 → remove(helper) → Access denied，同样短暂
//
// 这不是「偶发 IO 错误」而是「删刚跑过的 exe」在 Windows 上的固有行为，所以重试
// 是正解，不是掩盖症状：不重试的后果是回滚失败（服务停在新版本起不来的状态）或
// helper 永久残留（每更新一次漏一个文件）。
//
// 文件不存在视为成功（幂等）；预算耗尽返回最后一次错误，由调用方决定记日志还是上抛。
func removeFileRetrying(path string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	wait := 20 * time.Millisecond
	for {
		err := os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(wait)
		if wait < 160*time.Millisecond {
			wait *= 2
		}
	}
}

// replaceBinaryWithRollback installs stagePath beside targetPath and keeps the
// previous binary at targetPath+".old" until the caller confirms the new
// process started. The restore callback is idempotent and best-effort.
func replaceBinaryWithRollback(stagePath, targetPath string) (restore func() error, cleanup func(), err error) {
	backup := targetPath + ".old"
	if err := removeFileRetrying(backup, exeReleaseBudget); err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err := os.Rename(targetPath, backup); err != nil {
		return nil, nil, err
	}
	restored := false
	restore = func() error {
		if restored {
			return nil
		}
		// 这里删的 targetPath 是**刚被 exec 过**的新版本：进程已退出，但 Windows
		// 释放映像 section 有几十毫秒延迟，直接 os.Remove 会 Access denied（实测
		// 首次必败、50~150ms 后一次重试即成功）。回滚失败 = 用户被留在「新版本起
		// 不来、旧版本还叫 .old」的半坏状态，所以必须重试而不是把首个错误抛出去。
		if err := removeFileRetrying(targetPath, exeReleaseBudget); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(backup, targetPath); err != nil {
			return err
		}
		restored = true
		return nil
	}
	cleanup = func() {
		// 成功路径删备份：backup 是替换前那个进程的映像，可能同样还在释放中。
		// 失败不再静默吞（旧实现 `_ =`）：残留 .old 会一直躺在程序目录里。
		if err := removeFileRetrying(backup, exeReleaseBudget); err != nil {
			log.Printf("WARN: [updater] 清理回滚备份 %s 失败: %v", backup, err)
		}
	}
	if err := os.Rename(stagePath, targetPath); err != nil {
		return nil, nil, errors.Join(err, restore())
	}
	return restore, cleanup, nil
}

func isTrustedGitHubURL(u *url.URL, initial bool) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if initial {
		return host == "github.com"
	}
	return host == "github.com" ||
		strings.HasSuffix(host, ".github.com") ||
		host == "githubusercontent.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

func isContainer() bool {
	if strings.EqualFold(os.Getenv("container"), "docker") || strings.EqualFold(os.Getenv("container"), "podman") {
		return true
	}
	for _, path := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func verifySHA256(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, expected) {
		return fmt.Errorf("更新文件 SHA-256 不匹配: got %s", got)
	}
	return nil
}

func hasSHA256Digest(digest string) bool {
	if len(digest) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	return err == nil
}

func extractBinary(archivePath, stagePath, expectedName string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("打开更新压缩包失败: %w", err)
	}
	defer zr.Close()
	for _, file := range zr.File {
		if file.Name != expectedName {
			continue
		}
		if file.UncompressedSize64 == 0 || file.UncompressedSize64 > maxBinarySize || !file.Mode().IsRegular() {
			return errors.New("更新压缩包内的程序文件无效")
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_TRUNC, 0o700)
		if err != nil {
			src.Close()
			return err
		}
		n, copyErr := io.Copy(dst, io.LimitReader(src, maxBinarySize+1))
		srcErr, dstErr := src.Close(), dst.Close()
		if copyErr != nil {
			return fmt.Errorf("解压更新文件: %w", copyErr)
		}
		if srcErr != nil {
			return srcErr
		}
		if dstErr != nil {
			return dstErr
		}
		if n == 0 || n > maxBinarySize || uint64(n) != file.UncompressedSize64 {
			return errors.New("解压后的程序文件大小无效")
		}
		return nil
	}
	return fmt.Errorf("更新压缩包中未找到 %s", expectedName)
}

func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(dstPath)
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(dstPath)
		return err
	}
	return dst.Close()
}

// RestartAfter gives the HTTP handler time to flush its accepted response.
func (m *Manager) RestartAfter(delay time.Duration) {
	m.mu.RLock()
	fn := m.restarting
	m.mu.RUnlock()
	if fn == nil {
		return
	}
	time.AfterFunc(delay, fn)
}

// Run checks at startup, refreshes status every six hours, and installs once at
// the configured local (Shanghai) time when automatic updates are enabled.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		autoDue, regularDue := m.scheduleDue(m.now())
		if autoDue || regularDue {
			if err := m.Check(ctx); err != nil {
				if ctx.Err() == nil {
					if autoDue {
						log.Printf("WARN: [updater] 自动更新检查失败，今日自动更新未执行（明日自动时间再试）: %v", err)
					} else {
						log.Printf("WARN: [updater] release check: %v", err)
					}
				}
			} else if autoDue && m.Status().UpdateAvailable && m.Status().CanInstall {
				if err := m.Install(ctx); err != nil {
					log.Printf("WARN: [updater] automatic install: %v", err)
				} else {
					m.RestartAfter(time.Second)
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) scheduleDue(now time.Time) (auto, regular bool) {
	now = now.In(shanghai())
	m.mu.Lock()
	defer m.mu.Unlock()
	day := now.Format("2006-01-02")
	// 窗口式而非分钟精确相等：分钟 ticker 的相位漂移、上一轮 Check 占住循环跨过
	// 目标分钟、进程在目标分钟之后才启动，精确相等都会让当天自动更新静默落空。
	// 改为「今天还没跑过 且 已到/过目标时刻」——错过起点也能补跑当天的一次。
	target, terr := time.Parse("15:04", m.settings.AutoTime)
	auto = m.settings.AutoEnabled && terr == nil && m.lastAutoDay != day &&
		now.Hour()*60+now.Minute() >= target.Hour()*60+target.Minute()
	if auto {
		m.lastAutoDay = day
	}
	return auto, m.lastCheck.IsZero() || now.Sub(m.lastCheck) >= 6*time.Hour
}

func (m *Manager) canInstallLocked() bool {
	_, versioned := parseVersion(m.version)
	return versioned &&
		(runtime.GOOS == "linux" || runtime.GOOS == "windows") &&
		(runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") &&
		m.executable != "" && !isContainer() && !processSupervised()
}

func parseVersion(tag string) (parsedVersion, bool) {
	text := strings.TrimPrefix(tag, "v")
	parts := strings.Split(text, ".")
	if len(parts) != 4 || len(parts[3]) < 3 || len(parts[3]) > 4 {
		return parsedVersion{}, false
	}
	values := make([]int, 4)
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || (i < 3 && strconv.Itoa(n) != part) {
			return parsedVersion{}, false
		}
		values[i] = n
	}
	year, month, day := values[0], values[1], values[2]
	if year < 2000 || year > 9999 {
		return parsedVersion{}, false
	}
	timeCode := parts[3]
	hour, _ := strconv.Atoi(timeCode[:len(timeCode)-2])
	minute, _ := strconv.Atoi(timeCode[len(timeCode)-2:])
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return parsedVersion{}, false
	}
	if strconv.Itoa(hour)+fmt.Sprintf("%02d", minute) != timeCode {
		return parsedVersion{}, false
	}
	d := time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.UTC)
	if int(d.Month()) != month || d.Day() != day || d.Year() != year {
		return parsedVersion{}, false
	}
	ordinal := int64(year)*100000000 + int64(month)*1000000 + int64(day)*10000 + int64(hour*100+minute)
	return parsedVersion{text: text, ordinal: ordinal}, true
}

func shanghai() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err == nil {
		return loc
	}
	return time.FixedZone("Asia/Shanghai", 8*60*60)
}
