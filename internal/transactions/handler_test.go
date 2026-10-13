package transactions

import (
	"testing"
	"time"
)

func TestValidateKind(t *testing.T) {
	for _, kind := range []string{"income", "expense", "transfer"} {
		if err := validateKind(kind); err != nil {
			t.Fatalf("validateKind(%q) = %v, want nil", kind, err)
		}
	}
	if err := validateKind("refund"); err == nil {
		t.Fatal("validateKind(\"refund\") accepted an unknown kind")
	}
}

func TestSameDay(t *testing.T) {
	base := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

	if !sameDay(base, base.Add(2*time.Hour)) {
		t.Fatal("same day at a different time should be equal")
	}
	if sameDay(base, base.Add(24*time.Hour+time.Minute)) {
		t.Fatal("a minute after the next day should not be equal")
	}
	next := time.Date(2026, time.January, 16, 0, 0, 0, 0, time.UTC)
	if sameDay(next, base) {
		t.Fatal("consecutive days must differ")
	}
}
