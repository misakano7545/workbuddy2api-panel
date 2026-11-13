package auth

import (
	"testing"
	"time"
)

// TestParseNormalizesOfficialCLIExpiresAt 覆盖官方 CLI 凭据文件的导入：
// 官方写 expiresAt 为**毫秒**，本仓内部按 Unix 秒。不归一会让到期时间落到公元
// 58800 年 → NeedsRefresh 恒 false → token 永不刷新 → 到期后账号静默 401。
func TestParseNormalizesOfficialCLIExpiresAt(t *testing.T) {
	// 取自真实官方凭据文件（2026-10-24，www.codebuddy.ai）。
	const cliMillis = 1795254516410
	wantSec := int64(cliMillis / 1000)

	raw := []byte(`{
		"account": {"uid":"00000000-0000-4000-8000-000000000001","nickname":"misakano7545","uin":"000000000000"},
		"auth": {"accessToken":"at","refreshToken":"rt","expiresAt":1795254516410,
		         "domain":"www.codebuddy.ai","tokenType":"Bearer","expiresIn":2418995}
	}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.ExpiresAt != wantSec {
		t.Errorf("ExpiresAt=%d want %d（毫秒未归一）", a.ExpiresAt, wantSec)
	}
	// 归一后必须落在合理区间：距今不到 1 年（官方 expiresIn 约 28 天）。
	if d := time.Until(time.Unix(a.ExpiresAt, 0)); d <= 0 || d > 400*24*time.Hour {
		t.Errorf("归一后到期时间不合理：%v（now+%v）", time.Unix(a.ExpiresAt, 0), d)
	}
	// 官方国际站域必须判 global（否则错域路由）。
	if got := a.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
	// NeedsRefresh：28 天后到期的 token 现在不该被要求刷新。
	if a.NeedsRefresh(time.Hour) {
		t.Error("NeedsRefresh=true，说明到期时间仍被当成毫秒（未来过远）或已过期")
	}
}

// TestParseKeepsSeconds 覆盖旧格式：秒制 expiresAt 必须原样保留（不能被误除以 1000）。
func TestParseKeepsSeconds(t *testing.T) {
	const sec = int64(1795254516)
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1795254516,"domain":"www.codebuddy.cn"}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.ExpiresAt != sec {
		t.Errorf("ExpiresAt=%d want %d（秒制被误改）", a.ExpiresAt, sec)
	}
}

// TestParseFlatFormNormalizesToo 覆盖扁平形（手写 auth 文件）：两条解析分支都必须归一。
func TestParseFlatFormNormalizesToo(t *testing.T) {
	raw := []byte(`{"uid":"u1","accessToken":"at","refreshToken":"rt","expiresAt":1795254516410,"domain":"www.workbuddy.ai"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.ExpiresAt != 1795254516 {
		t.Errorf("ExpiresAt=%d want 1795254516", a.ExpiresAt)
	}
	if got := a.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
}

// TestNormalizeExpiresAt 覆盖阈值边界：秒制最大合理值不动，毫秒制一律 /1000。
func TestNormalizeExpiresAt(t *testing.T) {
	cases := []struct {
		in   int64
		want int64
	}{
		{in: 0, want: 0},
		{in: 1, want: 1},
		{in: 1795254516, want: 1795254516}, // 秒制当前值
		{in: expiresAtMillisThreshold, want: expiresAtMillisThreshold}, // 恰在阈值上：按秒（不越过才换算）
		{in: expiresAtMillisThreshold + 1, want: (expiresAtMillisThreshold + 1) / 1000},
		{in: 1795254516410, want: 1795254516}, // 毫秒制当前值
	}
	for _, c := range cases {
		if got := normalizeExpiresAt(c.in); got != c.want {
			t.Errorf("normalizeExpiresAt(%d)=%d want %d", c.in, got, c.want)
		}
	}
}
