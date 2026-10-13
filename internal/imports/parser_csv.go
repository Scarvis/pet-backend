// CSV statement parsing (§8.6 step c): header-driven when the first row names
// known columns, positional (date, amount, description) otherwise.
package imports

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"time"

	"github.com/Scarvis/pet-backend/internal/common"
)

// ParsedRow is one normalized statement line from any format. AmountDecimal
// keeps the exact decimal text ("-1 234,56"); the mapper converts it to minor
// units with the row or account currency. FITID is the bank transaction id
// (OFX); empty for CSV rows.
type ParsedRow struct {
	OccurredAt    time.Time `json:"occurred_at"`
	AmountDecimal string    `json:"amount"`
	Currency      string    `json:"currency,omitempty"`
	Description   string    `json:"description,omitempty"`
	FITID         string    `json:"fitid,omitempty"`
}

// dateLayouts tried for CSV dates, most specific first.
var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02",
	"20060102",
	"02.01.2006",
	"02/01/2006",
	"01/02/2006",
	"2.1.2006",
}

// parseDate tries each known layout, rejecting empty input.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, common.BadRequest("empty date")
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, common.BadRequest(fmt.Sprintf("unrecognized date %q", s))
}

// headerNames maps lower-cased column names to ParsedRow fields.
func headerIndex(header []string) map[string]int {
	idx := map[string]int{}
	for i, h := range header {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "date", "posted", "dtposted", "occurred_at", "booking_date":
			idx["date"] = i
		case "amount", "trnamt", "sum", "value":
			idx["amount"] = i
		case "description", "memo", "name", "details", "payee":
			idx["description"] = i
		case "currency", "curdef", "cur":
			idx["currency"] = i
		case "fitid", "id", "transaction_id":
			idx["fitid"] = i
		}
	}
	return idx
}

// looksLikeHeader reports whether row names at least a date and an amount.
func looksLikeHeader(row []string) bool {
	idx := headerIndex(row)
	_, okDate := idx["date"]
	_, okAmount := idx["amount"]
	return okDate && okAmount
}

// ParseCSV parses statement content into rows. With a header, columns map by
// name (extra NAME/MEMO-style columns are ignored); without one, columns are
// date, amount, description. Empty lines and rows with an empty amount are
// skipped; anything else malformed is an error.
func ParseCSV(content []byte) ([]ParsedRow, error) {
	content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF}) // strip BOM
	r := csv.NewReader(bytes.NewReader(content))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, common.Wrap(fmt.Errorf("csv parse: %w", err))
	}
	col := map[string]int{"date": 0, "amount": 1, "description": 2}
	if len(records) > 0 && looksLikeHeader(records[0]) {
		col = headerIndex(records[0])
		records = records[1:]
	}
	amountIdx, ok := col["amount"]
	if !ok {
		return nil, common.Wrap(common.BadRequest("csv has no amount column"))
	}
	var out []ParsedRow
	for n, rec := range records {
		if len(rec) == 0 {
			continue
		}
		get := func(key string) string {
			i, ok := col[key]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		amount := get("amount")
		if amount == "" {
			continue // blank line or running-balance filler
		}
		if amountIdx < len(rec) && strings.TrimSpace(rec[amountIdx]) == "" {
			continue
		}
		dateStr := get("date")
		var occurred time.Time
		if dateStr == "" {
			return nil, common.Wrap(common.BadRequest(fmt.Sprintf("csv row %d: missing date", n+1)))
		}
		occurred, err = parseDate(dateStr)
		if err != nil {
			return nil, common.Wrap(fmt.Errorf("csv row %d: %w", n+1, err))
		}
		out = append(out, ParsedRow{
			OccurredAt:    occurred,
			AmountDecimal: amount,
			Currency:      strings.ToUpper(get("currency")),
			Description:   get("description"),
		})
	}
	return out, nil
}
