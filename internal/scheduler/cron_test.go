package scheduler

import (
	"testing"
	"time"
)

func TestParseAndNext(t *testing.T) {
	loc := time.FixedZone("BRT", -3*3600)
	base := time.Date(2026, 11, 25, 10, 15, 30, 0, loc)

	tests := []struct {
		spec string
		want time.Time
	}{
		{"@daily", time.Date(2026, 11, 26, 0, 0, 0, 0, loc)},
		{"@hourly", time.Date(2026, 11, 25, 11, 0, 0, 0, loc)},
		{"@every 15m", base.Add(15 * time.Minute)},
		{"30 14 * * *", time.Date(2026, 11, 25, 14, 30, 0, 0, loc)},
		{"15 0 11 * * MON-FRI", time.Date(2026, 11, 25, 11, 0, 15, 0, loc)},
		{"0 0 1 * *", time.Date(2026, 12, 1, 0, 0, 0, 0, loc)},
		{"0 3 * * 7", time.Date(2026, 11, 29, 3, 0, 0, 0, loc)}, // Sunday via 7
	}

	for _, tc := range tests {
		sched, err := Parse(tc.spec)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", tc.spec, err)
		}
		got := sched.Next(base)
		if !got.Equal(tc.want) {
			t.Errorf("Parse(%q).Next(%v) = %v, want %v", tc.spec, base, got, tc.want)
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	invalid := []string{"", "* * *", "@every -5s", "60 * * * *", "* * * 13 *", "abc * * * *"}
	for _, spec := range invalid {
		if _, err := Parse(spec); err == nil {
			t.Errorf("expected Parse(%q) to fail", spec)
		}
	}
}
