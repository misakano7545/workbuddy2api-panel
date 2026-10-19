// 代理池健康度回写的回归测试（包内测试，直接打 reportProxyResult）。
//
// 背景：proxypool.Pool.Report 在 10-19 之前**从未被生产代码调用**，
// 导致 entry.ok/fail 恒 0、dead 恒 false，整套「连续 3 次失败自动熔断」
// 形同虚设（实测 23:45 单节点 16 个账号齐刷刷 EOF，面板仍显示 fail=0）。
//
// 本文件锁定三条语义边界：
//  1. 传输层失败（Do 返回 err / 读 body 断流）→ 喂熔断，3 次后 dead；
//  2. HTTP 业务错误（4xx/5xx）→ 算出口健康，**绝不能**把节点打死
//     （否则一次批量限流就能熔断全部 22 个出口，比不熔断更糟）；
//  3. 成功一次即清零失败 streak（间歇抖动不该累积成熔断）。
package upstream

import (
	"context"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
)

// resetDefaultPool 把全局默认池恢复成「纯净 off 模式」，避免测试间互相污染。
// 注：本文件不使用 t.Parallel()——Default() 是进程级单例。
func resetDefaultPool(t *testing.T) {
	t.Helper()
	p := proxypool.Default()
	p.Configure(nil, "off")
	t.Cleanup(func() { p.Configure(nil, "off") })
}

// TestReportProxyResultTransportFailureTripsBreaker 三次传输层失败 → 出口 dead。
func TestReportProxyResultTransportFailureTripsBreaker(t *testing.T) {
	resetDefaultPool(t)
	p := proxypool.Default()
	p.Configure([]string{"http://127.0.0.1:1"}, "per_account")

	// 先让 Pick 建立 uid→出口 绑定（生产里由 Transport.Proxy 完成）
	if _, raw := p.Pick("uid-breaker"); raw == "" {
		t.Fatal("Pick 应建立绑定")
	}
	if got := p.Peek("uid-breaker"); got == "" {
		t.Fatal("Peek 应能反查到绑定出口（否则 reportProxyResult 永远 no-op）")
	}

	ctx := proxypool.WithUID(context.Background(), "uid-breaker")
	for i := 1; i <= 3; i++ {
		reportProxyResult(ctx, false)
		snap := p.Snapshot()
		if len(snap) != 1 {
			t.Fatalf("snapshot len=%d want 1", len(snap))
		}
		if snap[0].Fail != int64(i) {
			t.Errorf("第 %d 次失败后 fail=%d want %d", i, snap[0].Fail, i)
		}
		if i < 3 && snap[0].Dead {
			t.Errorf("第 %d 次失败不该熔断（阈值 3）", i)
		}
	}

	if snap := p.Snapshot(); !snap[0].Dead {
		t.Fatalf("连续 3 次传输失败后应 dead: %+v", snap[0])
	}
}

// TestReportProxyResultBusinessErrorKeepsEgressHealthy 业务错误不熔断出口。
func TestReportProxyResultBusinessErrorKeepsEgressHealthy(t *testing.T) {
	resetDefaultPool(t)
	p := proxypool.Default()
	p.Configure([]string{"http://127.0.0.1:2"}, "per_account")
	p.Pick("uid-healthy")

	// 模拟 10 次 4xx/5xx（429/401/402 等业务判定）
	ctx := proxypool.WithUID(context.Background(), "uid-healthy")
	for i := 0; i < 10; i++ {
		reportProxyResult(ctx, true)
	}

	snap := p.Snapshot()
	if snap[0].Dead {
		t.Fatalf("业务错误绝不能熔断出口（一次批量限流会打死全部节点）: %+v", snap[0])
	}
	if snap[0].OK != 10 || snap[0].Fail != 0 {
		t.Errorf("ok=%d fail=%d want 10/0", snap[0].OK, snap[0].Fail)
	}
}

// TestReportProxyResultSuccessClearsFailStreak 成功一次即清零失败 streak。
func TestReportProxyResultSuccessClearsFailStreak(t *testing.T) {
	resetDefaultPool(t)
	p := proxypool.Default()
	p.Configure([]string{"http://127.0.0.1:3"}, "per_account")
	p.Pick("uid-flap")

	ctx := proxypool.WithUID(context.Background(), "uid-flap")
	reportProxyResult(ctx, false)
	reportProxyResult(ctx, false)
	if snap := p.Snapshot(); snap[0].Fail != 2 {
		t.Fatalf("fail=%d want 2", snap[0].Fail)
	}
	reportProxyResult(ctx, true)
	if snap := p.Snapshot(); snap[0].Fail != 0 || snap[0].Dead {
		t.Fatalf("成功一次应清零失败 streak（间歇抖动不该累积成熔断）: %+v", snap[0])
	}
}

// TestReportProxyResultNoUIDIsNoop 无 uid / 未绑定 / off 模式必须静默 no-op。
func TestReportProxyResultNoUIDIsNoop(t *testing.T) {
	resetDefaultPool(t)

	// off 模式 + 无 uid：不应 panic、不应产生任何计数
	reportProxyResult(context.Background(), false)

	p := proxypool.Default()
	p.Configure([]string{"http://127.0.0.1:4"}, "per_account")
	// 有池但该 uid 从未 Pick 过（Peek 返回空）→ no-op
	reportProxyResult(proxypool.WithUID(context.Background(), "never-picked"), false)

	if snap := p.Snapshot(); len(snap) != 1 || snap[0].Fail != 0 || snap[0].OK != 0 {
		t.Fatalf("未绑定的 uid 不该产生健康度计数: %+v", snap)
	}
}
