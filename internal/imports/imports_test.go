package imports

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseCSVHeader(t *testing.T) {
	rows, err := ParseCSV([]byte("date,amount,description\n2026-01-05,-1234.56,Coffee\n"))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].AmountDecimal != "-1234.56" || rows[0].Description != "Coffee" {
		t.Fatalf("unexpected row: %+v", rows[0])
	}
	if rows[0].OccurredAt.Format("2006-01-02") != "2026-01-05" {
		t.Fatalf("bad date: %v", rows[0].OccurredAt)
	}
}

func TestParseCSVSkipsAmountlessRows(t *testing.T) {
	// A ragged first line without an amount column is skipped, not fatal.
	rows, err := ParseCSV([]byte("2026-02-01;no amount here\n2026-02-01,100.00,Salary\n"))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if len(rows) != 1 || rows[0].Description != "Salary" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

func TestParseOFX(t *testing.T) {
	// OFX 1.x leaf tags have no closing tags; the scanner reads them flat.
	raw := `<OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><CURDEF>USD<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260105120000<TRNAMT>-20.00<FITID>abc123<NAME>Shop<MEMO>groceries</STMTTRN></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`
	rows, err := ParseOFX([]byte(raw))
	if err != nil {
		t.Fatalf("ParseOFX: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.FITID != "abc123" || r.Currency != "USD" || r.AmountDecimal != "-20.00" {
		t.Fatalf("unexpected row: %+v", r)
	}
	if r.Description != "Shop — groceries" {
		t.Fatalf("unexpected description %q", r.Description)
	}
}

func TestDecimalToMinor(t *testing.T) {
	cases := map[string]int64{
		"10.00":    1000,
		"-10.00":   1000, // sign handled by caller; magnitude only
		"1,234.56": 123456,
		"1.234,56": 123456,
		"1 234,56": 123456,
		"0.01":     1,
	}
	for in, want := range cases {
		got, err := DecimalToMinor(in, 2)
		if err != nil {
			t.Fatalf("DecimalToMinor(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("DecimalToMinor(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := DecimalToMinor("0.001", 2); err == nil {
		t.Fatal("sub-cent amount should round to zero and fail")
	}
	if _, err := DecimalToMinor("abc", 2); err == nil {
		t.Fatal("non-numeric amount accepted")
	}
}

func TestDedupOccurrence(t *testing.T) {
	acc := uuid.New()
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	mk := func() ParsedRow { return ParsedRow{OccurredAt: day, AmountDecimal: "-5.00", Description: "Coffee"} }
	rows := []ParsedRow{mk(), mk()}
	occ := AssignOccurrences(rows)
	if occ[0] != 0 || occ[1] != 1 {
		t.Fatalf("occurrences = %v, want [0 1]", occ)
	}
	h0, h1 := DedupHash(acc, rows[0], occ[0]), DedupHash(acc, rows[1], occ[1])
	if h0 == h1 {
		t.Fatal("identical rows collapsed to one hash")
	}
	// FITID pins the hash regardless of occurrence.
	f := mk()
	f.FITID = "xyz"
	if DedupHash(acc, f, 0) != DedupHash(acc, f, 7) {
		t.Fatal("FITID hash must ignore the occurrence index")
	}
}
