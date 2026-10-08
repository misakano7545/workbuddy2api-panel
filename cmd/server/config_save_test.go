package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSaveConfigReportsActualAndPendingRestarts 重启清单 = 「运行中配置 vs 新配置」的
// 实际差异：改动即入列、第二处保存不丢上一处的待重启项、改回原值自动清除。
// 移植上游 PR #104（saveConfig 多传 running 基线；本 fork 多 gw/pn 两参传 nil）。
func TestSaveConfigReportsActualAndPendingRestarts(t *testing.T) {
	cfg := Default()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(livecfg.Snapshot{})
	p := pool.New("")
	defer p.Close()
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	save := func(body string) []string {
		t.Helper()
		fields, err := saveConfig([]byte(body), path, live, p, up, sch, nil, nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return fields
	}
	if fields := save(`{"api_key":"changed"}`); len(fields) != 0 {
		t.Fatalf("hot save restart fields=%v", fields)
	}
	if live.Load().APIKey != "changed" {
		t.Fatal("key was not applied immediately")
	}
	if fields := save(`{"listen":":9999"}`); !reflect.DeepEqual(fields, []string{"listen"}) {
		t.Fatalf("changed fields=%v", fields)
	}
	if fields := save(`{"api_key":"second"}`); !reflect.DeepEqual(fields, []string{"listen"}) {
		t.Fatalf("pending restart disappeared: %v", fields)
	}
	if fields := save(`{"listen":":7863"}`); len(fields) != 0 {
		t.Fatalf("reverted pending fields=%v", fields)
	}
}

// TestSaveConfigWhenDirNotWritable 目录不可写但 config.json 可写时仍要能保存：Docker 里
// /app 属主是镜像内的 app(10001)，而 compose 让容器以 PUID 运行 —— 原子替换的 tmp 建不
// 出来（issue #134），必须退回原地重写。
//
// root 不受目录权限位约束（CAP_DAC_OVERRIDE），所以本机 root 下 skip；CI 以普通用户跑，
// 这条才真的会红/绿。
func TestSaveConfigWhenDirNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("目录权限位对 root 无效，需非 root 运行才有牙齿（CI 生效）")
	}
	cfg := Default()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cfg)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil { // 目录不可写：tmp 建不出来
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	live := livecfg.New(livecfg.Snapshot{})
	p := pool.New("")
	defer p.Close()
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	if _, err := saveConfig([]byte(`{"api_key":"changed-in-place"}`), path, live, p, up, sch, nil, nil, cfg); err != nil {
		t.Fatalf("目录不可写时应原地重写而不是报错: %v", err)
	}
	if live.Load().APIKey != "changed-in-place" {
		t.Fatal("key was not applied immediately")
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatalf("落盘内容不是完整 JSON（原地重写写坏了？）: %v", err)
	}
	if got["api_key"] != "changed-in-place" {
		t.Fatalf("api_key=%v", got["api_key"])
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("不应残留 config.json.tmp")
	}
}

// TestRestartRequiredIncludesAllAssemblyFields 装配期字段（监听/身份/双域/提示词/
// 会话粘性/upstash）逐族抽查：变化必须被 restartRequiredFields 报告。
func TestRestartRequiredIncludesAllAssemblyFields(t *testing.T) {
	current, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseConfig([]byte(`{"global":{"enabled":false},"prompt":{"mode":"custom"},"upstream":{"user_agent":"new-agent"},"session_sticky":{"enabled":false},"server":{"read_timeout":"120s"},"upstash":{"url":"https://example.upstash.io","token":"sample"}}`))
	if err != nil {
		t.Fatal(err)
	}
	fields := restartRequiredFields(current, next)
	for _, want := range []string{"global.enabled", "prompt.mode", "upstream.user_agent", "session_sticky.enabled", "server.read_timeout", "upstash.url", "upstash.token"} {
		found := false
		for _, field := range fields {
			found = found || field == want
		}
		if !found {
			t.Fatalf("missing %s in %v", want, fields)
		}
	}
}

// TestSaveConfigRetainsEnvironmentManagedKey WB2A_API_KEY 托管时：保存提交的文件值
// 不得抢班（运行值恒为 env 值），且不产生重启项。移植上游 PR #104。
func TestSaveConfigRetainsEnvironmentManagedKey(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "environment-key")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"file-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(livecfg.Snapshot{APIKey: cfg.APIKey})
	p := pool.New("")
	defer p.Close()
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	fields, err := saveConfig([]byte(`{"api_key":"submitted-key"}`), path, live, p, up, sch, nil, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 0 || live.Load().APIKey != "environment-key" {
		t.Fatalf("fields=%v key changed=%v", fields, live.Load().APIKey != "environment-key")
	}
}

// TestEnvironmentManagedFieldsMatchesValidOverrides 托管清单只收「值合法」的 env：
// 非法值（WB2A_TIMEOUT_SECONDS=invalid）等同未设置，不进清单。
func TestEnvironmentManagedFieldsMatchesValidOverrides(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "env-key")
	t.Setenv("WB2A_SOFT_RATE", "30s")
	t.Setenv("WB2A_TIMEOUT_SECONDS", "invalid")
	fields := Default().EnvironmentManagedFields()
	has := func(want string) bool {
		for _, field := range fields {
			if field == want {
				return true
			}
		}
		return false
	}
	if !has("api_key") || !has("cooldown.soft_rate") || has("upstream.timeout_seconds") {
		t.Fatalf("managed=%v", fields)
	}
}
