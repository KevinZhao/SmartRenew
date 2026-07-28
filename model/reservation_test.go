package model

import (
	"testing"
	"time"
)

func TestDaysUntil(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		end  time.Time
		want int
	}{
		{"exactly 5 days", now.Add(5 * 24 * time.Hour), 5},
		{"5 days and 1 hour rounds up", now.Add(5*24*time.Hour + time.Hour), 6},
		{"2 hours left counts as a day", now.Add(2 * time.Hour), 1},
		{"1 second left counts as a day", now.Add(time.Second), 1},
		{"exactly now", now, 0},
		{"expired 1 hour ago", now.Add(-time.Hour), 0},
		{"expired 25 hours ago", now.Add(-25 * time.Hour), -1},
		{"expired exactly 2 days ago", now.Add(-48 * time.Hour), -2},
		{"zero end time", time.Time{}, 0},
		{"30 days", now.Add(30 * 24 * time.Hour), 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DaysUntil(tc.end, now); got != tc.want {
				t.Fatalf("DaysUntil(%v, %v) = %d, want %d", tc.end, now, got, tc.want)
			}
		})
	}
}

func TestDaysUntilIsTimezoneIndependent(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	end := now.Add(5 * 24 * time.Hour)

	utcDays := DaysUntil(end, now)
	offsetDays := DaysUntil(end.In(shanghai), now.In(shanghai))
	if utcDays != offsetDays {
		t.Fatalf("same instants gave different results: UTC=%d, +08:00=%d", utcDays, offsetDays)
	}
}

func TestCrossedThreshold(t *testing.T) {
	remind := []int{30, 14, 7, 3, 1}
	tests := []struct {
		name string
		days int
		want int
	}{
		{"exactly at 30", 30, 30},
		{"between 14 and 30", 20, 30},
		{"exactly at 14", 14, 14},
		{"between 7 and 14", 10, 14},
		{"exactly at 7", 7, 7},
		{"between 3 and 7", 5, 7},
		{"exactly at 3", 3, 3},
		{"between 1 and 3", 2, 3},
		{"exactly at 1", 1, 1},
		{"already expired", 0, 1},
		{"beyond the widest threshold", 60, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CrossedThreshold(tc.days, remind); got != tc.want {
				t.Fatalf("CrossedThreshold(%d, %v) = %d, want %d", tc.days, remind, got, tc.want)
			}
		})
	}
}

func TestCrossedThresholdUnsortedAndEmpty(t *testing.T) {
	// Config order must not matter.
	if got := CrossedThreshold(5, []int{1, 30, 7, 3, 14}); got != 7 {
		t.Errorf("unsorted thresholds: got %d, want 7", got)
	}
	// No thresholds configured → nothing to remind at.
	if got := CrossedThreshold(5, nil); got != 0 {
		t.Errorf("nil thresholds: got %d, want 0", got)
	}
}

func TestNewAlertRecordsEachConfiguredThreshold(t *testing.T) {
	// A resource must produce a distinct alert as it passes each configured
	// remind day, so "remind me at 30/14/7/3/1" actually fires five times.
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	remind := []int{30, 14, 7, 3, 1}

	seen := map[int]bool{}
	for _, days := range []int{30, 14, 7, 3, 1} {
		r := Reservation{EndTime: now.Add(time.Duration(days) * 24 * time.Hour)}
		a := NewAlert(r, now, remind)
		if a.Threshold != days {
			t.Errorf("at %d days: Threshold = %d, want %d", days, a.Threshold, days)
		}
		if seen[a.Threshold] {
			t.Errorf("threshold %d produced twice — dedup would collapse reminders", a.Threshold)
		}
		seen[a.Threshold] = true
	}
	if len(seen) != 5 {
		t.Fatalf("got %d distinct thresholds, want 5", len(seen))
	}
}

func TestCalcAlertLevel(t *testing.T) {
	tests := []struct {
		days int
		want AlertLevel
	}{
		{-5, LevelCritical},
		{0, LevelCritical},
		{1, LevelCritical},
		{3, LevelCritical},
		{4, LevelWarning},
		{7, LevelWarning},
		{8, LevelAttention},
		{14, LevelAttention},
		{15, LevelNormal},
		{30, LevelNormal},
		{365, LevelNormal},
	}
	for _, tc := range tests {
		if got := CalcAlertLevel(tc.days); got != tc.want {
			t.Errorf("CalcAlertLevel(%d) = %q, want %q", tc.days, got, tc.want)
		}
	}
}

func TestNewAlertDerivesDaysAndLevelConsistently(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	r := Reservation{ID: "x", EndTime: now.Add(2*24*time.Hour + 3*time.Hour)}

	a := NewAlert(r, now, nil)
	if a.DaysLeft != 3 {
		t.Fatalf("DaysLeft = %d, want 3 (2d3h rounds up)", a.DaysLeft)
	}
	if a.Level != LevelCritical {
		t.Fatalf("Level = %q, want critical for 3 days", a.Level)
	}
	// The level must always be what CalcAlertLevel says about DaysLeft; a
	// mismatch means the two were computed from different day counts.
	if a.Level != CalcAlertLevel(a.DaysLeft) {
		t.Fatalf("Level %q disagrees with CalcAlertLevel(%d) = %q", a.Level, a.DaysLeft, CalcAlertLevel(a.DaysLeft))
	}
	if a.ID != "x" {
		t.Errorf("embedded reservation not preserved: ID = %q", a.ID)
	}
}

func TestNewAlertLevelMatchesDaysAcrossRange(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for h := 1; h <= 24*40; h++ {
		r := Reservation{EndTime: now.Add(time.Duration(h) * time.Hour)}
		a := NewAlert(r, now, nil)
		if a.Level != CalcAlertLevel(a.DaysLeft) {
			t.Fatalf("at +%dh: Level=%q but CalcAlertLevel(%d)=%q", h, a.Level, a.DaysLeft, CalcAlertLevel(a.DaysLeft))
		}
		if a.DaysLeft < 1 {
			t.Fatalf("at +%dh: DaysLeft=%d, a future expiry must be at least 1", h, a.DaysLeft)
		}
	}
}
