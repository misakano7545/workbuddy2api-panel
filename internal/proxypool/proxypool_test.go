// Package proxypool_test proxypool 单元测试
package proxypool_test

import (
	"context"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
)

func TestProxyPool_ModesAndAssignment(t *testing.T) {
	p := proxypool.New()
	if p.Mode() != proxypool.ModeOff {
		t.Fatalf("expected ModeOff, got %s", p.Mode())
	}

	// 1. 配置 3 个代理 + 1 个非法协议
	proxies := []string{
		"http://1.1.1.1:8080",
		"socks5://user:pass123@2.2.2.2:1080",
		"socks5h://3.3.3.3:1080",
		"ftp://invalid-scheme:1234", // 协议不支持，应被跳过
	}
	skipped := p.Configure(proxies, "per_account")
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skipped, got %d: %v", len(skipped), skipped)
	}
	if p.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", p.Len())
	}
	if p.Mode() != proxypool.ModePerAccount {
		t.Fatalf("expected ModePerAccount, got %s", p.Mode())
	}

	// 2. per_account 模式：同一账号必须分配到相同的代理出口
	u1, raw1 := p.Pick("user-A")
	if u1 == nil || raw1 == "" {
		t.Fatalf("expected proxy for user-A")
	}
	u2, raw2 := p.Pick("user-A")
	if raw1 != raw2 || u1.String() != u2.String() {
		t.Fatalf("user-A should get stable proxy, got %s vs %s", raw1, raw2)
	}

	// 3. 密码脱敏检查
	snap := p.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 snapshot items, got %d", len(snap))
	}
	foundMasked := false
	for _, it := range snap {
		if it.Host == "2.2.2.2:1080" {
			if it.Raw != "socks5://user:***@2.2.2.2:1080" {
				t.Fatalf("expected masked raw, got %s", it.Raw)
			}
			foundMasked = true
		}
	}
	if !foundMasked {
		t.Fatalf("expected to find masked socks5 proxy in snapshot")
	}

	// 4. 连续失败达阈值熔断
	p.Report(raw1, false)
	p.Report(raw1, false)
	p.Report(raw1, false) // 3 次失败
	// user-A 绑定的出口 dead 后，重新请求应该能换到新的健康出口
	u3, raw3 := p.Pick("user-A")
	if u3 == nil || raw3 == raw1 {
		t.Fatalf("user-A should failover from dead proxy, got %s", raw3)
	}

	// 5. 重置熔断
	resetN := p.ResetDead()
	if resetN < 1 {
		t.Fatalf("expected at least 1 dead reset, got %d", resetN)
	}

	// 6. context 辅助
	ctx := context.Background()
	if proxypool.UIDFrom(ctx) != "" {
		t.Fatalf("empty ctx should return empty uid")
	}
	ctx = proxypool.WithUID(ctx, "uid-12345")
	if proxypool.UIDFrom(ctx) != "uid-12345" {
		t.Fatalf("expected uid-12345, got %s", proxypool.UIDFrom(ctx))
	}
}

func TestProxyPool_RoundRobin(t *testing.T) {
	p := proxypool.New()
	p.Configure([]string{"http://1.1.1.1:8080", "http://2.2.2.2:8080"}, "round_robin")
	if p.Mode() != proxypool.ModeRoundRobin {
		t.Fatalf("expected ModeRoundRobin, got %s", p.Mode())
	}

	_, r1 := p.Pick("user-A")
	_, r2 := p.Pick("user-A")
	if r1 == r2 {
		t.Fatalf("round robin should rotate even for same uid, got %s and %s", r1, r2)
	}
}
