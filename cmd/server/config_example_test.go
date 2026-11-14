package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestConfigExampleIsValid 守卫仓库根目录的 config.example.json 是合法 JSON、
// 且能被 Config 结构解析（键名拼错/漏逗号都不会被编译期发现）。
//
// 来由：PR #55 加 features.sanitize_brand_scope 时漏了上一行的逗号，
// 文件变成非法 JSON 而 CI 全绿——因为它只在运行时被 `cp config.example.json
// config.json` 的人读到，报错现场离提交很远。这个测试把现场挪回 CI。
func TestConfigExampleIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	if !json.Valid(raw) {
		t.Fatalf("%s 不是合法 JSON", path)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s 无法解析为 Config: %v", path, err)
	}
	// 新增的对外配置项必须在示例里可见，否则用户只能从 README 反推。
	if c.Features.SanitizeBrandScope != "all" {
		t.Errorf("features.sanitize_brand_scope 示例值应为 all，得到 %q", c.Features.SanitizeBrandScope)
	}
	if !c.Features.SanitizeBlacklistFingerprints {
		t.Error("features.sanitize_blacklist_fingerprints 示例值应为 true")
	}
	if c.Pool.ModelFallback == nil {
		t.Error("pool.model_fallback 示例应存在（空对象即可），否则用户不知道有这张表")
	}
	// models.allowed（issue #68）：示例必须是存在的数组（仿上方各项的「示例可见」
	// 口径）。空数组解析为非 nil 空切片；nil = 键缺失，用户无从发现白名单配置。
	if c.Models.Allowed == nil {
		t.Error("models.allowed 示例应存在（空数组即可），否则用户不知道有白名单")
	}
	// 6020/6021 排队等待的默认值也要在示例里可见（PR #217）。
	if !c.Features.BusyRetry || c.Features.BusyWaitSeconds != 60 {
		t.Errorf("features.busy_retry / busy_wait_seconds 示例应为 true / 60: %+v", c.Features)
	}
}
