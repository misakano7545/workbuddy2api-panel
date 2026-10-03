package pool

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 移植上游 PR #104：小数积分保留 + 零成本观测可信标记。

func exactCreditsForTest(p *Pool, uid string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byUID[uid].exactCredits()
}

// TestFractionalCreditChargesPersistAndAuthoritativeUpdatesResetRemainder
// 小数扣费不丢：15×0.1 后精确余额 99.5（整数口径显示 100）；跨重启保号；
// 权威余额整写（SetCredits/SetCreditsDetailed/ReenableIfCredits）清零余量。
func TestFractionalCreditChargesPersistAndAuthoritativeUpdatesResetRemainder(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	p := New(statePath)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 101, 0)
	for i := 0; i < 15; i++ {
		p.NoteModelCost("u1", "fractional", 0.1, 1000)
	}
	if got := exactCreditsForTest(p, "u1"); got != 99.5 {
		t.Fatalf("exact balance after 15 x 0.1 charges = %v, want 99.5", got)
	}
	if status, ok := p.Status("u1"); !ok || status.Credits != 100 {
		t.Fatalf("integer external balance should remain rounded: status=%+v ok=%v", status, ok)
	}
	p.Flush()
	p.Close()

	restored := New(statePath)
	defer restored.Close()
	restored.Add(&auth.Auth{UID: "u1"})
	if got := exactCreditsForTest(restored, "u1"); got != 99.5 {
		t.Fatalf("restored exact balance = %v, want 99.5", got)
	}

	restored.SetCredits("u1", 200, 0)
	if got := exactCreditsForTest(restored, "u1"); got != 200 {
		t.Fatalf("SetCredits must reset fractional remainder, got %v", got)
	}
	restored.NoteModelCost("u1", "fractional", 0.1, 1000)
	if got := exactCreditsForTest(restored, "u1"); got != 199.9 {
		t.Fatalf("fractional charge after SetCredits = %v, want 199.9", got)
	}

	restored.SetCreditsDetailed("u1", 70, 70, 0, time.Time{}, 0)
	if got := exactCreditsForTest(restored, "u1"); got != 70 {
		t.Fatalf("SetCreditsDetailed must reset fractional remainder, got %v", got)
	}
	restored.NoteModelCost("u1", "fractional", 0.1, 1000)
	restored.ReenableIfCredits("u1", 50, 0)
	if got := exactCreditsForTest(restored, "u1"); got != 50 {
		t.Fatalf("ReenableIfCredits must reset fractional remainder, got %v", got)
	}
}

// TestConfirmedFreeObservationPersistsVerification 确认免费（credit 显式=0）
// 的观测带可信标记落盘，并跨重启恢复为 tier 0 证据。
func TestConfirmedFreeObservationPersistsVerification(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	p := New(statePath)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 0)
	p.NoteModelCost("u1", "free", 0, 1000)
	p.Flush()
	p.Close()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved stateFile
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Accounts["u1"].ModelCosts["free"].CreditVerified {
		t.Fatal("new zero-cost observation must be persisted with verified credit provenance")
	}

	restored := New(statePath)
	defer restored.Close()
	restored.Add(&auth.Auth{UID: "u1"})
	status, ok := restored.Status("u1")
	if !ok || len(status.ModelCosts) != 1 || status.ModelCosts[0].CostPer1k != 0 {
		t.Fatalf("verified free observation should survive restart, status=%+v ok=%v", status, ok)
	}
}

// TestCreditFloorUsesExactFractionalBalance 保底判定看精确余额：整数口径还等于
// 保底线、实际已跌破（101 → 100.9）时，付费模型必须被拦住。
func TestCreditFloorUsesExactFractionalBalance(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	defer p.Close()
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(101)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 101, 0)
	p.NoteModelCost("u1", "paid", 0.1, 1000)

	if status, ok := p.Status("u1"); !ok || status.Credits != 101 {
		t.Fatalf("legacy integer field should still show 101: status=%+v ok=%v", status, ok)
	}
	if got := exactCreditsForTest(p, "u1"); got != 100.9 {
		t.Fatalf("exact balance = %v, want 100.9", got)
	}
	if a := p.PickExcludingForRealm(nil, "paid", ""); a != nil {
		t.Fatalf("exact balance below floor must block the paid model even while integer field equals floor, got %v", a.UID)
	}
}

// TestLegacyZeroCostMigratesButVerifiedFreeAndLegacyPaidRemain 旧版「无标记的零价
// 观测」一次性迁移清除（脏免费证据）；已验证免费与旧正价记录保留，其余账号状态不动。
func TestLegacyZeroCostMigratesButVerifiedFreeAndLegacyPaidRemain(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	sf := stateFile{Accounts: map[string]stateAccount{
		"old-zero": {
			Credits:    321,
			TokenUsage: TokenUsage{RequestCount: 7, TotalTokens: 44},
			ModelCosts: map[string]stateModelCost{
				"legacy-free": {CostPer1k: 0, LastSeen: now, Samples: 3},
			},
		},
		"confirmed-zero": {
			Credits: 222,
			ModelCosts: map[string]stateModelCost{
				"verified-free": {CostPer1k: 0, LastSeen: now, Samples: 2, CreditVerified: true},
			},
		},
		"old-paid": {
			Credits: 111,
			ModelCosts: map[string]stateModelCost{
				"legacy-paid": {CostPer1k: 1.25, LastSeen: now, Samples: 5},
			},
		},
	}}
	raw, err := json.Marshal(sf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(statePath)
	defer p.Close()
	for _, uid := range []string{"old-zero", "confirmed-zero", "old-paid"} {
		p.Add(&auth.Auth{UID: uid})
	}

	oldZero, ok := p.Status("old-zero")
	if !ok || oldZero.Credits != 321 || oldZero.TokenUsage.RequestCount != 7 || oldZero.TokenUsage.TotalTokens != 44 {
		t.Fatalf("migration must preserve other account state: status=%+v ok=%v", oldZero, ok)
	}
	if len(oldZero.ModelCosts) != 0 {
		t.Fatalf("legacy zero without verified source must become unknown, got %+v", oldZero.ModelCosts)
	}
	confirmedZero, ok := p.Status("confirmed-zero")
	if !ok || len(confirmedZero.ModelCosts) != 1 || confirmedZero.ModelCosts[0].CostPer1k != 0 {
		t.Fatalf("verified free observation should restore, got %+v ok=%v", confirmedZero, ok)
	}
	oldPaid, ok := p.Status("old-paid")
	if !ok || len(oldPaid.ModelCosts) != 1 || math.Abs(oldPaid.ModelCosts[0].CostPer1k-1.25) > 1e-12 {
		t.Fatalf("legacy positive observation should restore, got %+v ok=%v", oldPaid, ok)
	}
	if !p.dirty.Load() {
		t.Fatal("dropping legacy zero observations should dirty state for one-time migration persistence")
	}
}
