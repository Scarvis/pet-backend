package invest

import (
	"math/big"
	"testing"
)

func rat(s string) *big.Rat {
	q, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad test quantity " + s)
	}
	return q
}

func TestApplyBuyAverage(t *testing.T) {
	// 10 units @ 100 + 10 units @ 200 → 20 @ 150.
	newQty, newAvg := applyBuy(rat("10"), rat("10"), 100, 200)
	if newQty.Cmp(rat("20")) != 0 {
		t.Fatalf("qty = %s, want 20", newQty.RatString())
	}
	if newAvg != 150 {
		t.Fatalf("avg = %d, want 150", newAvg)
	}
}

func TestApplyBuyFractional(t *testing.T) {
	// 0.5 @ 100 + 0.5 @ 300 → 1 @ 200.
	newQty, newAvg := applyBuy(rat("0.5"), rat("0.5"), 100, 300)
	if newQty.Cmp(rat("1")) != 0 {
		t.Fatalf("qty = %s, want 1", newQty.RatString())
	}
	if newAvg != 200 {
		t.Fatalf("avg = %d, want 200", newAvg)
	}
}

func TestFormatQuantity(t *testing.T) {
	cases := map[string]string{"10": "10", "0.50": "0.5", "1.123456789012": "1.123456789"}
	for in, want := range cases {
		if got := formatQuantity(rat(in)); got != want {
			t.Fatalf("formatQuantity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseQuantityRejects(t *testing.T) {
	for _, s := range []string{"0", "-1.5", "abc", ""} {
		if _, err := parseQuantity(s); err == nil {
			t.Fatalf("parseQuantity(%q) accepted an invalid quantity", s)
		}
	}
}

func TestHoldingValueMinor(t *testing.T) {
	v, err := HoldingValueMinor("2.5", 400)
	if err != nil {
		t.Fatalf("HoldingValueMinor: %v", err)
	}
	if v != 1000 {
		t.Fatalf("value = %d, want 1000", v)
	}
	if _, err := HoldingValueMinor("nan", 100); err == nil {
		t.Fatal("HoldingValueMinor accepted a bad quantity")
	}
}
