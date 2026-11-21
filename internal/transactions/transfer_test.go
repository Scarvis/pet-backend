package transactions

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

type fakeAccountRow struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	Currency string
}

type fakeAccountRows struct {
	rows  []fakeAccountRow
	next  int
	scan  error
	fetch error
}

func (f *fakeAccountRows) Next() bool {
	if f.next >= len(f.rows) {
		return false
	}
	f.next++
	return true
}

func (f *fakeAccountRows) Scan(dest ...any) error {
	if f.scan != nil {
		return f.scan
	}
	row := f.rows[f.next-1]
	*(dest[0].(*uuid.UUID)) = row.ID
	*(dest[1].(*uuid.UUID)) = row.OrgID
	*(dest[2].(*string)) = row.Currency
	return nil
}

func (f *fakeAccountRows) Err() error {
	return f.fetch
}

func (f *fakeAccountRows) Close() {}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}

func TestScanTransferAccountsMatchesByIDNotRowOrder(t *testing.T) {
	fromID := mustUUID(t, "e9feb8ea-46f4-4dc3-bb9c-3893572f3953")
	toID := mustUUID(t, "5c9709b4-bfc1-41c5-94d2-5635d66e629a")
	orgID := mustUUID(t, "11111111-1111-1111-1111-111111111111")

	// Ascending id order puts the target first; the lookup must still tell them apart.
	rows := &fakeAccountRows{rows: []fakeAccountRow{
		{ID: toID, OrgID: orgID, Currency: "USD"},
		{ID: fromID, OrgID: orgID, Currency: "RUB"},
	}}

	from, to, err := scanTransferAccounts(rows, fromID, toID)
	if err != nil {
		t.Fatalf("scanTransferAccounts() error = %v, want nil", err)
	}
	if from.ID != fromID || from.Currency != "RUB" {
		t.Fatalf("from = %+v, want id %s currency RUB", from, fromID)
	}
	if to.ID != toID || to.Currency != "USD" {
		t.Fatalf("to = %+v, want id %s currency USD", to, toID)
	}
}

func TestScanTransferAccountsRejectsMissingTarget(t *testing.T) {
	fromID := mustUUID(t, "e9feb8ea-46f4-4dc3-bb9c-3893572f3953")
	toID := mustUUID(t, "5c9709b4-bfc1-41c5-94d2-5635d66e629a")

	rows := &fakeAccountRows{rows: []fakeAccountRow{
		{ID: fromID, Currency: "RUB"},
	}}

	_, _, err := scanTransferAccounts(rows, fromID, toID)
	if err == nil || !strings.Contains(err.Error(), "target account not found") {
		t.Fatalf("error = %v, want target account not found", err)
	}
}

func TestScanTransferAccountsRejectsMissingSource(t *testing.T) {
	fromID := mustUUID(t, "e9feb8ea-46f4-4dc3-bb9c-3893572f3953")
	toID := mustUUID(t, "5c9709b4-bfc1-41c5-94d2-5635d66e629a")

	rows := &fakeAccountRows{rows: []fakeAccountRow{
		{ID: toID, Currency: "RUB"},
	}}

	_, _, err := scanTransferAccounts(rows, fromID, toID)
	if err == nil || !strings.Contains(err.Error(), "source account not found") {
		t.Fatalf("error = %v, want source account not found", err)
	}
}
