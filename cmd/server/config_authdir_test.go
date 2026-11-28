package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("WB2A_NO_AUTH_MIGRATION", "1")
	os.Exit(m.Run())
}

func isolateUserDirs(t *testing.T, root string) {
	t.Helper()
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("HOME", root)
	t.Setenv("WB2A_AUTH_DIR", "")
	t.Setenv("WB2A_NO_AUTH_MIGRATION", "")
	t.Chdir(root)
}

func writeConfigFile(t *testing.T, path, authDir string) {
	t.Helper()
	content := `{}`
	if authDir != "" {
		content = `{"auth_dir":"` + filepath.ToSlash(authDir) + `"}`
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMigratesLegacyAuthDirs(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		oldDir  func() string
		useJSON bool
	}{
		{"program dir", "workbuddy-test.json", func() string { return "auths" }, false},
		{"user root", "workbuddy-prev.json", legacyUserAuthDir, true},
		{"nested user dir", "workbuddy-nested.json", legacyNestedAuthDir, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			isolateUserDirs(t, root)

			old := tc.oldDir()
			if old == "" {
				t.Skip("no user config dir on this platform")
			}
			if err := os.MkdirAll(old, 0o700); err != nil {
				t.Fatal(err)
			}
			oldFile := filepath.Join(old, tc.file)
			if err := os.WriteFile(oldFile, []byte(`{"uid":"test"}`), 0o600); err != nil {
				t.Fatal(err)
			}

			cfgAuthDir := ""
			if tc.useJSON {
				cfgAuthDir = old
			}
			cfgPath := filepath.Join(root, "config.json")
			writeConfigFile(t, cfgPath, cfgAuthDir)
			c, err := Load(cfgPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			want := defaultAuthDir()
			if c.AuthDir != want {
				t.Fatalf("auth_dir=%q want %q", c.AuthDir, want)
			}
			if _, err := os.Stat(filepath.Join(want, tc.file)); err != nil {
				t.Fatalf("legacy account was not migrated: %v", err)
			}
			if _, err := os.Stat(oldFile); err != nil {
				t.Fatalf("legacy original should be kept: %v", err)
			}
		})
	}
}

func TestExplicitAuthDirIsNotOverridden(t *testing.T) {
	t.Setenv("WB2A_AUTH_DIR", "")
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-auths")
	cfgPath := filepath.Join(dir, "config.json")
	writeConfigFile(t, cfgPath, custom)

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if filepath.ToSlash(c.AuthDir) != filepath.ToSlash(custom) {
		t.Fatalf("auth_dir=%q want %q", c.AuthDir, custom)
	}
}
