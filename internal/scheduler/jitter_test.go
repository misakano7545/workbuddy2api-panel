package scheduler

import (
	"testing"
	"time"
)

func TestJitterIsStableAndInsideWindow(t *testing.T) {
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.Local)
	a := nextFireKind(now, []int{9}, taskCheckin, 30)
	b := nextFireKind(now, []int{9}, taskCheckin, 30)
	if !a.Equal(b) {
		t.Fatalf("jitter must be stable: %v vs %v", a, b)
	}
	nominal := time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)
	if a.Before(nominal) || !a.Before(nominal.Add(30*time.Minute)) {
		t.Fatalf("offset %v outside [9:00, 9:30)", a)
	}
	if nextFireKind(now, []int{9}, taskCheckin, 0) != nominal {
		t.Fatal("jitter 0 must stay on the hour")
	}
	if nextFireKind(now, []int{9}, taskTravel, 30).Equal(a) {
		t.Fatal("different tasks should not share one offset")
	}
}
