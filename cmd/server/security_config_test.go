package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/scheduler"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// issue #244：黑名单条目写错必须保存时就拒（点名第几项），不能静默跳过——
// 运维以为封了、实际一条没生效，比不给这个功能更危险。
func TestNormalizeIPBlacklistValidation(t *testing.T) {
	ok := Default()
	ok.Security.IPBlacklist = []string{"104.233.189.37", " 45.12.0.0/16 ", "", "2001:db8::1"}
	if err := ok.normalize(); err != nil {
		t.Fatalf("合法条目被拒: %v", err)
	}
	bad := Default()
	bad.Security.IPBlacklist = []string{"104.233.189.37", "45.12.0.0/33"}
	err := bad.normalize()
	if err == nil {
		t.Fatal("非法 CIDR 应当报错")
	}
	if !strings.Contains(err.Error(), "第 2 项") {
		t.Fatalf("错误信息应点名第几项: %v", err)
	}
}

// 面板保存 security.ip_blacklist 必须热生效（livecfg 快照重建那一处最容易漏接：
// 少接一条，面板显示已保存、实际照旧放行）。
func TestSaveConfigAppliesIPBlacklist(t *testing.T) {
	cfg := Default()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(livecfg.Snapshot{})
	p := pool.New("")
	defer p.Close()
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	if _, err := saveConfig([]byte(`{"security":{"ip_blacklist":["104.233.189.37","45.12.0.0/16"]}}`),
		path, live, p, up, sch, nil, nil, cfg); err != nil {
		t.Fatal(err)
	}
	snap := live.Load()
	if !snap.IPBlacklist.Blocks("104.233.189.37") || !snap.IPBlacklist.Blocks("45.12.9.9") {
		t.Fatalf("热生效失败: %+v", snap.IPBlacklist)
	}
	if snap.IPBlacklist.Blocks("8.8.8.8") {
		t.Fatal("未命中条目不该被拦")
	}
}
