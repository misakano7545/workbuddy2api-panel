package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// AdoptNewer 只在磁盘令牌更新时原地改；静态 API Key（过期时刻都为 0）以令牌不同为准。
func TestAdoptNewer(t *testing.T) {
	cases := []struct {
		name       string
		curAT      string
		curExp     int64
		srcAT      string
		srcExp     int64
		wantAdopt  bool
		wantAccess string
	}{
		{"磁盘更晚过期", "at1", 1000, "at2", 2000, true, "at2"},
		{"磁盘更早过期", "at1", 2000, "at2", 1000, false, "at1"},
		{"同令牌", "at1", 1000, "at1", 2000, false, "at1"},
		{"磁盘令牌为空", "at1", 1000, "", 2000, false, "at1"},
		{"静态 key 换了", "ck_old", 0, "ck_new", 0, true, "ck_new"},
	}
	for _, c := range cases {
		cur := &Auth{UID: "u", AccessToken: c.curAT, ExpiresAt: c.curExp}
		src := &Auth{UID: "u", AccessToken: c.srcAT, ExpiresAt: c.srcExp, Nickname: "n"}
		if got := cur.AdoptNewer(src); got != c.wantAdopt || cur.AccessToken != c.wantAccess {
			t.Errorf("%s: adopt=%v access=%q want %v/%q", c.name, got, cur.AccessToken, c.wantAdopt, c.wantAccess)
		}
	}
}

// 目录不存在视为空目录（面板首次登录前 auths/ 还没建）；读不动则必须报错，
// 否则热重载会把「读不动」当成「0 个文件」把整池删光。
func TestLoadDirReportDirErrors(t *testing.T) {
	r, err := LoadDirReport(filepath.Join(t.TempDir(), "nope"))
	if err != nil || r.Files != 0 {
		t.Fatalf("不存在的目录应视为空：files=%d err=%v", r.Files, err)
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("需要能让目录不可读的非 root POSIX 环境")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := LoadDirReport(dir); err == nil {
		t.Fatal("不可读目录应返回错误")
	}
}
