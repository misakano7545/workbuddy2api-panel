package server

import "testing"

func TestDailyBudgetStopsAtLimit(t *testing.T) {
	b := newDailyBudget(1)
	b.add(0.4, true)
	if !b.admit() {
		t.Fatal("under limit should pass")
	}
	b.add(0.6, true)
	if b.admit() {
		t.Fatal("used == limit should reject")
	}
	used, limit, rejected := b.snapshot()
	if used != 1 || limit != 1 || rejected != 1 {
		t.Fatalf("snapshot used=%v limit=%v rejected=%d", used, limit, rejected)
	}
	b.add(-3, true)
	b.add(5, false)
	used, _, _ = b.snapshot()
	if used != 1 {
		t.Fatalf("negative and unobserved must not move used, got %v", used)
	}
}
