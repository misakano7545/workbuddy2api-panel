package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if handled, err := HandleSelfUpdate(os.Args); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fixtureGraceMS 集成测试用的健康观察窗口（真值 8s，见 childGraceWindow）。
// 缩到 400ms：这条用例要覆盖三条路径，8s 会让每个分支多等 8 秒。
const fixtureGraceMS = 400

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The fixture process exits after staging, allowing its real helper to replace
// the executable and start the replacement with the saved arguments.
func TestInstallFixtureProcess(t *testing.T) {
	dir := os.Getenv("WB2API_UPDATE_FIXTURE")
	if dir == "" {
		t.Skip("subprocess fixture")
	}
	archive, err := os.ReadFile(filepath.Join(dir, "release.zip"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	m := New("2026.10.22.1823", "example/project", "", Settings{})
	m.SetRestart(func() {})
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := archive
		if r.URL.Host == "api.github.com" {
			body, err = json.Marshal([]release{{
				TagName: "v2026.10.23.1859",
				Assets: []releaseAsset{{
					Name:               fmt.Sprintf("wb2api-panel-2026.10.23.1859-%s-%s.zip", runtime.GOOS, runtime.GOARCH),
					BrowserDownloadURL: "https://github.com/example/project/releases/download/test/file.zip",
					Size:               int64(len(archive)),
					Digest:             "sha256:" + hex.EncodeToString(sum[:]),
				}},
			}})
			if err != nil {
				return nil, err
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Header: make(http.Header)}, nil
	})
	os.Args = []string{os.Args[0], "-test.run=^TestRestartFixtureProcess$", "-test.timeout=20s"}
	if err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.Status().Installing {
		t.Fatal("installation must stay pending until shutdown")
	}
	if err := m.Install(context.Background()); err == nil {
		t.Fatal("duplicate install accepted")
	}
}

// TestRestartFixtureProcess 被更新流程拉起的那一侧（新版本/回滚后的旧版本）。
// 行为由 WB2API_UPDATE_FIXTURE_MODE 决定，对应三条要覆盖的路径：
//
//	healthy 活过观察窗口才退出 —— 只有这样健康检查才会放行、cleanup 才删备份
//	crashes 立刻退出 —— 触发「启动即崩 → 回滚」（删刚跑过的 exe 的那条路径）
//	broken  永远不会被拉起（压缩包里不是可执行文件）
func TestRestartFixtureProcess(t *testing.T) {
	dir := os.Getenv("WB2API_UPDATE_FIXTURE")
	if dir == "" {
		t.Skip("subprocess fixture")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "restarted.txt"), []byte(cwd+"\n"+strings.Join(os.Args[1:], "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	switch os.Getenv("WB2API_UPDATE_FIXTURE_MODE") {
	case "healthy":
		// 必须活过 childGrace（400ms）才算「新版本能跑」。
		time.Sleep(2 * fixtureGraceMS * time.Millisecond)
	case "crashes":
		os.Exit(3)
	}
}

var updateProcessTestMu sync.Mutex

func TestInstallReplacesRestartsAndRecovers(t *testing.T) {
	if !New("2026.10.22.1823", "", "", Settings{}).Status().CanInstall {
		t.Skip("native self-update unavailable on this deployment")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	// 三条路径，各自确定性覆盖（旧版只有 broken false/true 两档，而 false 档因为
	// 重启进程秒退，实际**一直在走回滚**——健康路径零覆盖，且回滚里的 Windows
	// 句柄延迟释放没人测出来，表现为 ~30% 的「backup not removed」）。
	//
	//	healthy  新版本活过观察窗口 → cleanup 删备份、helper 被清理（真正的成功路径）
	//	crashes  新版本起来后立刻退出 → 回滚（本次修的那条：删刚跑过的 exe 要重试）
	//	broken   压缩包里根本不是可执行文件 → CreateProcess 失败 → 回滚
	for _, mode := range []string{"healthy", "crashes", "broken"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("WB2API_UPDATE_FIXTURE", dir)
			t.Setenv("WB2API_UPDATE_FIXTURE_MODE", mode)
			// helper 是独立进程，观察窗口只能经环境变量注入（见 updater.childGrace）。
			// 缩到 400ms：真值 8s 会让这条用例每个分支多等 8 秒。
			t.Setenv("WB2API_UPDATE_GRACE_MS", strconv.Itoa(fixtureGraceMS))
			target := filepath.Join(dir, "updater.test"+filepath.Ext(executable))
			if err := os.WriteFile(target, original, 0700); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			// 发布包固定使用 wb2api 名称，不能用测试二进制的扩展名（Go 1.26 为 .test）。
			binaryName := "wb2api"
			if runtime.GOOS == "windows" {
				binaryName += ".exe"
			}
			header := &zip.FileHeader{Name: binaryName, Method: zip.Deflate}
			header.SetMode(0o700)
			entry, err := zw.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			next := original
			if mode == "broken" {
				next = []byte("not an executable")
			}
			if _, err := entry.Write(next); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "release.zip"), buf.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}

			// 夹具进程：调 Install 排好 helper 后退出（helper 等它退出才替换文件）。
			cmd := exec.Command(executable, "-test.run=^TestInstallFixtureProcess$", "-test.timeout=40s")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"WB2API_UPDATE_FIXTURE="+dir,
				"WB2API_UPDATE_FIXTURE_MODE="+mode,
				"WB2API_UPDATE_GRACE_MS="+strconv.Itoa(fixtureGraceMS),
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("fixture: %v\n%s", err, output)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				content, readErr := os.ReadFile(filepath.Join(dir, "restarted.txt"))
				helpers, _ := filepath.Glob(filepath.Join(dir, ".wb2api-*"))
				if readErr == nil && len(helpers) == 0 {
					if !strings.HasPrefix(string(content), dir+"\n") || !strings.Contains(string(content), "-test.run=^TestRestartFixtureProcess$") {
						t.Fatalf("cwd/arguments not preserved: %q", content)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("restart/cleanup incomplete: err=%v helpers=%v\n%s", readErr, helpers, output)
				}
				time.Sleep(25 * time.Millisecond)
			}
			// healthy 分支里重启出来的进程要活过观察窗口才算成功，它退出得比
			// 「helper 被清掉」更晚；等它走完，否则 t.TempDir 的 RemoveAll 在
			// Windows 上会因映像占用失败，把一条通过的用例变成失败。
			if mode == "healthy" {
				time.Sleep(3 * fixtureGraceMS * time.Millisecond)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("installed/restored executable differs: %v", err)
			}
			if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
				t.Fatalf("backup not removed: %v", err)
			}
			leftovers, _ := filepath.Glob(filepath.Join(dir, ".wb2api-*"))
			if len(leftovers) != 0 {
				t.Fatalf("更新残留未清理: %v", leftovers)
			}
		})
	}
}
