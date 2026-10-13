package events

import (
	"testing"

	"github.com/google/uuid"
)

func TestParseTransactionPayload(t *testing.T) {
	org := uuid.New()
	cat := uuid.New()
	raw := []byte(`{"org_id":"` + org.String() + `","category_id":"` + cat.String() + `","kind":"expense","occurred_at":"2026-10-13T00:00:00Z"}`)
	p, err := parseTransactionPayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.OrgID != org || p.CategoryID == nil || *p.CategoryID != cat || p.Kind != "expense" {
		t.Fatalf("bad parse: %+v", p)
	}

	// NULL category (transfers) is legal.
	raw = []byte(`{"org_id":"` + org.String() + `","category_id":null,"kind":"transfer","occurred_at":"2026-10-13T00:00:00Z"}`)
	p, err = parseTransactionPayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.CategoryID != nil {
		t.Fatalf("category must be nil: %+v", p)
	}

	if _, err := parseTransactionPayload([]byte(`{"kind":"expense"}`)); err == nil {
		t.Fatal("missing org_id must fail")
	}
	if _, err := parseTransactionPayload([]byte(`not json`)); err == nil {
		t.Fatal("invalid json must fail")
	}
}
