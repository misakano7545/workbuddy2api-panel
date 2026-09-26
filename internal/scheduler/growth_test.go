package scheduler

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestNextWakeGrowthIndependent 成长任务队列有独立时点（默认 11:00）。
func TestNextWakeGrowthIndependent(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{9},
		TravelHours:    []int{21},
		ActivityHours:  []int{10},
		KeepaliveHours: []int{22},
		BlackcatHours:  []int{23},
		GrowthHours:    []int{11},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 10, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 11, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（成长队列 11:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskGrowth {
		t.Errorf("kinds=%v want [growth]", kinds)
	}
}

// TestNextWakeGrowthDisabled 关掉后不再有成长时点（其他任务照常）。
func TestNextWakeGrowthDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		GrowthHours:       []int{11},
		GrowthDisabled:    true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 10, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（成长禁用 → 跳过 11:00）", at, want)
	}
	if hasKind(kinds, taskGrowth) {
		t.Errorf("kinds=%v 不应含 growth（已禁用）", kinds)
	}
}

// TestRunGrowthNowRunner 执行器调用语义：未注入静默不 panic，注入后每次触发调用一次。
func TestRunGrowthNowRunner(t *testing.T) {
	s := New(Config{})
	s.RunGrowthNow() // 未注入（nil）：静默返回

	var calls, lastTotal int32
	s.SetGrowthRunner(func() (int, string) {
		atomic.AddInt32(&calls, 1)
		return 3, ""
	})
	s.RunGrowthNow()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("runner 调用次数=%d want 1", got)
	}
	s.SetGrowthRunner(func() (int, string) {
		atomic.AddInt32(&lastTotal, 1)
		return 0, "全部账号没有待办任务"
	})
	s.RunGrowthNow()
	if got := atomic.LoadInt32(&lastTotal); got != 1 {
		t.Errorf("替换后的 runner 调用次数=%d want 1", got)
	}
}

// TestReconfigureGrowthHot 热改：时点与开关即时生效（面板保存配置的路径）。
func TestReconfigureGrowthHot(t *testing.T) {
	s := New(Config{GrowthHours: []int{11}})
	off := ScheduleParams{
		TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true, BlackcatDisabled: true,
	}
	p := off
	p.GrowthHours = []int{13}
	s.Reconfigure(p)
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 11, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 13, 0, 0, 0, time.Local); !at.Equal(want) || !hasKind(kinds, taskGrowth) {
		t.Errorf("next=%v kinds=%v want 13:00 [growth]（时点热改）", at, kinds)
	}

	p.GrowthDisabled = true
	s.Reconfigure(p)
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 11, 30, 0, 0, time.Local))
	if hasKind(kinds, taskGrowth) {
		t.Errorf("kinds=%v 不应含 growth（开关热改）", kinds)
	}
}
