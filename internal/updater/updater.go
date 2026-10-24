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

// Settings controls the optional daily automatic install.
type Settings struct {
	AutoEnabled bool
	AutoTime    string
}

// Status is safe to expose through the authenticated panel API.
type Status struct {
	CurrentVersion  string `json:"current_version"`
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
	m.status.CanInstall = m.canInstallLocked()
	return m
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
			m.status.UpdateAvailable = !currentOK || newestVersion.ordinal > current.ordinal
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
	return nil
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
	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxArchiveSize+1))
	if err != nil {
		return fmt.Errorf("写入更新文件失败: %w", err)
	}
	if n > maxArchiveSize {
		return errors.New("下载文件超过大小限制")
	}
	if asset.Size > 0 && n != asset.Size {
		return fmt.Errorf("下载文件大小不符: got %d, want %d", n, asset.Size)
	}
	return nil
}

// HandleSelfUpdate handles the private helper invocation used by Install.
// It returns handled=false for normal server startup.
func HandleSelfUpdate(args []string) (handled bool, err error) {
	if len(args) >= 4 && args[1] == cleanupArg {
		pid, parseErr := strconv.Atoi(args[2])
		if parseErr != nil || pid <= 0 {
			return true, errors.New("清理进程参数中的 pid 无效")
		}
		if err := waitForProcessExit(pid, 60*time.Second); err != nil {
			return true, err
		}
		return true, os.Remove(args[3])
	}
	if len(args) < 7 || args[1] != selfUpdateArg {
		return false, nil
	}
	pid, err := strconv.Atoi(args[2])
	if err != nil || pid <= 0 {
		return true, errors.New("更新进程参数中的 pid 无效")
	}
	stagePath, targetPath, cwd, originalArgsJSON := args[3], args[4], args[5], args[6]
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
	startTarget := func() error {
		cmd := exec.Command(targetPath, originalArgs...)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		return startChild(cmd)
	}
	restore, cleanup, err := replaceBinaryWithRollback(stagePath, targetPath)
	if err != nil {
		restartErr := startTarget()
		_ = os.Remove(stagePath)
		cleanupHelper()
		return true, errors.Join(fmt.Errorf("替换程序文件: %w", err), restartErr)
	}
	_ = os.Remove(stagePath)
	if err := startTarget(); err != nil {
		restoreErr := restore()
		if restoreErr == nil {
			restoreErr = startTarget()
		}
		cleanupHelper()
		return true, errors.Join(fmt.Errorf("启动新版本: %w", err), restoreErr)
	}
	cleanup()
	cleanupHelper()
	return true, nil
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

// replaceBinaryWithRollback installs stagePath beside targetPath and keeps the
// previous binary at targetPath+".old" until the caller confirms the new
// process started. The restore callback is idempotent and best-effort.
func replaceBinaryWithRollback(stagePath, targetPath string) (restore func() error, cleanup func(), err error) {
	backup := targetPath + ".old"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
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
		if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(backup, targetPath); err != nil {
			return err
		}
		restored = true
		return nil
	}
	cleanup = func() {
		_ = os.Remove(backup)
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
					log.Printf("WARN: [updater] release check: %v", err)
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
	auto = m.settings.AutoEnabled && now.Format("15:04") == m.settings.AutoTime && m.lastAutoDay != day
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
