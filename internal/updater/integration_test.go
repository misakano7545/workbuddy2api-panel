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
	"strings"
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
}

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
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("broken=%v", broken), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("WB2API_UPDATE_FIXTURE", dir)
			target := filepath.Join(dir, "wb2api"+filepath.Ext(executable))
			if err := os.WriteFile(target, original, 0700); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			entry, err := zw.Create(filepath.Base(target))
			if err != nil {
				t.Fatal(err)
			}
			next := original
			if broken {
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
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, target, "-test.run=^TestInstallFixtureProcess$", "-test.timeout=20s")
			cmd.Dir = dir
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
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("installed/restored executable differs: %v", err)
			}
			if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
				t.Fatalf("backup not removed: %v", err)
			}
		})
	}
}
