package budgets

import (
	"testing"
	"time"
)

func TestPeriodBoundsMonthly(t *testing.T) {
	startsOn := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	ref := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	start, end, err := PeriodBounds("monthly", startsOn, ref)
	if err != nil {
		t.Fatalf("PeriodBounds: %v", err)
	}
	if want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("start = %v, want %v", start, want)
	}
	if end.IsZero() || !end.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %v, want 2026-04-01", end)
	}
}

func TestPeriodBoundsWeeklyMonday(t *testing.T) {
	startsOn := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 2026-03-18 is a Wednesday; week must start Monday 2026-03-16.
	ref := time.Date(2026, 3, 18, 15, 0, 0, 0, time.UTC)
	start, end, err := PeriodBounds("weekly", startsOn, ref)
	if err != nil {
		t.Fatalf("PeriodBounds: %v", err)
	}
	if want := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("start = %v, want %v", start, want)
	}
	if end.IsZero() || !end.Equal(time.Date(2026, 3, 23, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %v, want 2026-03-23", end)
	}
}

func TestPeriodBoundsCustom(t *testing.T) {
	startsOn := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	start, end, err := PeriodBounds("custom", startsOn, time.Now().UTC())
	if err != nil {
		t.Fatalf("PeriodBounds: %v", err)
	}
	if !start.Equal(startsOn) {
		t.Fatalf("start = %v, want %v", start, startsOn)
	}
	// period_end is NOT NULL in the schema, so custom uses a far-future
	// sentinel as its open-ended marker.
	if want := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Fatalf("custom end = %v, want %v", end, want)
	}
}

func TestPeriodBoundsBadPeriod(t *testing.T) {
	if _, _, err := PeriodBounds("yearly", time.Now().UTC(), time.Now().UTC()); err == nil {
		t.Fatal("PeriodBounds accepted an unknown period")
	}
}

func TestShouldAlert(t *testing.T) {
	if !ShouldAlert(80, 100, 80) {
		t.Fatal("80/100 at 80% should alert")
	}
	if ShouldAlert(79, 100, 80) {
		t.Fatal("79/100 at 80% should not alert")
	}
	if ShouldAlert(1000, 0, 80) {
		t.Fatal("zero limit must never alert")
	}
}
