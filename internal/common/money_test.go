package common

import "testing"

func TestConvertToMinorRoundTrip(t *testing.T) {
	// $100 (minor_unit 2) at rate 2 -> 200 human units of a minor_unit-0 currency.
	got, err := ConvertToMinor(10_000, 2, 2, 0)
	if err != nil {
		t.Fatalf("ConvertToMinor: %v", err)
	}
	if got != 200 {
		t.Fatalf("expected 200 minor units, got %d", got)
	}

	// Same minor_unit on both sides just scales by the rate.
	got, err = ConvertToMinor(10_000, 2, 2, 2)
	if err != nil {
		t.Fatalf("ConvertToMinor: %v", err)
	}
	if got != 20_000 {
		t.Fatalf("expected 20000 minor units, got %d", got)
	}
}

func TestFromMinor(t *testing.T) {
	human, err := FromMinor(20_000, 2) // 200.00
	if err != nil {
		t.Fatalf("FromMinor: %v", err)
	}
	if human != 200 {
		t.Fatalf("expected 200, got %f", human)
	}

	// High-decimal currency (BTC minor_unit 8): 0.001 BTC -> 100_000 satoshis.
	satoshis, err := ToMinor(0.001, 8)
	if err != nil {
		t.Fatalf("ToMinor: %v", err)
	}
	if satoshis != 100_000 {
		t.Fatalf("expected 100000 satoshis, got %d", satoshis)
	}

	back, err := FromMinor(satoshis, 8)
	if err != nil {
		t.Fatalf("FromMinor: %v", err)
	}
	if back != 0.001 {
		t.Fatalf("expected 0.001, got %f", back)
	}
}

func TestMoneyRejectsNegative(t *testing.T) {
	if _, err := ToMinor(-1, 2); err == nil {
		t.Fatal("ToMinor accepted a negative amount")
	}
	if _, err := ConvertToMinor(100, -1, 2, 2); err == nil {
		t.Fatal("ConvertToMinor accepted a negative rate")
	}
}

func TestBigintParsing(t *testing.T) {
	b, err := Bigint("9223372036854775807").Int64() // max int64
	if err != nil {
		t.Fatalf("parse max: %v", err)
	}
	if b != 9223372036854775807 {
		t.Fatalf("unexpected value: %d", b)
	}

	if _, err := Bigint("99999999999999999999").Int64(); err == nil {
		t.Fatal("parse out-of-range int64 did not error")
	}

	neg, err := Bigint("-5").Int64()
	if err != nil || neg != -5 {
		t.Fatalf("expected -5, got %d (%v)", neg, err)
	}
}

func TestBigintJSON(t *testing.T) {
	data, err := Bigint("123").MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(data) != `"123"` {
		t.Fatalf("expected quoted decimal string, got %s", data)
	}

	var b Bigint
	if err := b.UnmarshalJSON([]byte(`"456"`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if b != Bigint("456") {
		t.Fatalf("expected 456, got %s", b)
	}

	var bad Bigint
	if err := bad.UnmarshalJSON([]byte(`123`)); err == nil {
		t.Fatal("UnmarshalJSON accepted a non-string value")
	}
}

func TestMinorUnit(t *testing.T) {
	cases := map[string]int{"JPY": 0, "BTC": 8, "ETH": 18, "USD": 2, "EUR": 2}
	for code, want := range cases {
		if got := MinorUnit(code); got != want {
			t.Fatalf("MinorUnit(%q) = %d, want %d", code, got, want)
		}
	}
}
