package main

import (
	"encoding/json"
	"testing"
)

func TestParseNumeric(t *testing.T) {
	// JSON string form (Hasura numeric as string).
	got, err := parseNumeric(json.RawMessage(`"12.5"`))
	if err != nil || got != "12.5" {
		t.Fatalf("string quantity: got %q err %v", got, err)
	}
	// JSON number form.
	got, err = parseNumeric(json.RawMessage(`12.5`))
	if err != nil || got != "12.5" {
		t.Fatalf("number quantity: got %q err %v", got, err)
	}
	// Missing / null / blank must fail.
	for _, raw := range []string{``, `null`, `"  "`} {
		if _, err := parseNumeric(json.RawMessage(raw)); err == nil {
			t.Fatalf("quantity %q must fail", raw)
		}
	}
	// Non-numeric must fail.
	if _, err := parseNumeric(json.RawMessage(`true`)); err == nil {
		t.Fatal("boolean quantity must fail")
	}
}

func TestToBigint(t *testing.T) {
	if got := toBigint(12345); string(got) != "12345" {
		t.Fatalf("got %q", got)
	}
	if got := toBigint(-7); string(got) != "-7" {
		t.Fatalf("got %q", got)
	}
}
