// Row mapping (§8.6 step e): decimal → minor units, duplicate lookup, and
// INSERT of tx.transactions (source='import') with the same ledger + balance
// effect as a manual transaction (§8.2), so imported money moves balances.
package imports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Scarvis/pet-backend/internal/common"
)

// normalizeDecimal unifies decimal separators: with both ',' and '.' present
// the last one is the fraction separator ("1,234.56" vs "1.234,56"); with
// only ',' it is the fraction separator ("1234,56"); spaces are thousands
// separators and are dropped.
func normalizeDecimal(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	s = strings.ReplaceAll(s, "\u00a0", "")
	lastComma := strings.LastIndex(s, ",")
	lastDot := strings.LastIndex(s, ".")
	switch {
	case lastComma >= 0 && lastDot >= 0:
		if lastComma > lastDot {
			s = strings.ReplaceAll(s, ".", "")
			s = strings.ReplaceAll(s, ",", ".")
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	case lastComma >= 0:
		s = strings.ReplaceAll(s, ",", ".")
	}
	return s
}

// DecimalToMinor converts a decimal amount string to minor units of a currency
// with the given minor_unit, truncating toward zero. A zero or negative result
// is an error: tx.transactions requires amount_minor > 0 (§3.2), and the sign
// decides income vs expense before this is called.
func DecimalToMinor(decimal string, minorUnit int) (int64, error) {
	r, ok := new(big.Rat).SetString(normalizeDecimal(decimal))
	if !ok {
		return 0, common.BadRequest(fmt.Sprintf("invalid amount %q", decimal))
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(minorUnit)), nil)
	v := new(big.Rat).Mul(r, new(big.Rat).SetInt(scale))
	out := new(big.Int).Quo(v.Num(), v.Denom())
	if !out.IsInt64() {
		return 0, common.BadRequest(fmt.Sprintf("amount %q overflows int64", decimal))
	}
	minor := out.Int64()
	if minor == 0 {
		return 0, common.BadRequest(fmt.Sprintf("amount %q rounds to zero", decimal))
	}
	if minor < 0 {
		minor = -minor
	}
	return minor, nil
}

// minorUnitOf returns fx.currencies.minor_unit for code, falling back to the
// compiled-in default for codes missing from the table.
func minorUnitOf(ctx context.Context, tx pgx.Tx, code string) (int, error) {
	var mu int
	err := tx.QueryRow(ctx, `SELECT minor_unit FROM fx.currencies WHERE code = $1`, code).Scan(&mu)
	if errors.Is(err, pgx.ErrNoRows) {
		return common.MinorUnit(code), nil
	}
	if err != nil {
		return 0, common.Wrap(err)
	}
	return mu, nil
}

// nearestRate returns the most recent rate for base→quote on or before asOf,
// mirroring transactions.nearestRate (§8.2 step 3).
func nearestRate(ctx context.Context, tx pgx.Tx, base, quote string, asOf time.Time) (float64, error) {
	var rate float64
	err := tx.QueryRow(ctx, `SELECT rate FROM fx.rates WHERE base_code = $1 AND quote_code = $2 AND as_of <= $3::date ORDER BY as_of DESC LIMIT 1`, base, quote, asOf).Scan(&rate)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, common.NotFound(fmt.Sprintf("no fx rate for %s→%s", base, quote))
	}
	if err != nil {
		return 0, common.Wrap(err)
	}
	return rate, nil
}

// MapRow maps one parsed row to a transaction (§8.6 step e). A zero amount is
// rejected; the sign decides kind (positive → income, negative → expense) and
// amount_minor stores the absolute value. The row currency wins when it names
// a known currency, otherwise the account currency applies; differing
// currencies convert at the nearest historical rate (§8.2 step 3). Balance and
// ledger effects join the caller's transaction.
func MapRow(ctx context.Context, tx pgx.Tx, orgID, accountID, jobID uuid.UUID, row ParsedRow) (uuid.UUID, error) {
	r, ok := new(big.Rat).SetString(normalizeDecimal(row.AmountDecimal))
	if !ok {
		return uuid.Nil, common.Wrap(common.BadRequest(fmt.Sprintf("invalid amount %q", row.AmountDecimal)))
	}
	if r.Sign() == 0 {
		return uuid.Nil, common.Wrap(common.BadRequest("zero amounts cannot be imported"))
	}
	kind := "income"
	if r.Sign() < 0 {
		kind = "expense"
	}

	var accCurrency string
	if err := tx.QueryRow(ctx, `SELECT currency FROM account.accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, accountID).Scan(&accCurrency); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, common.Wrap(common.NotFound("account not found"))
		}
		return uuid.Nil, common.Wrap(err)
	}
	currency := accCurrency
	if row.Currency != "" && row.Currency != accCurrency {
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fx.currencies WHERE code = $1)`, row.Currency).Scan(&known); err != nil {
			return uuid.Nil, common.Wrap(err)
		}
		if known {
			currency = row.Currency
		}
	}
	mu, err := minorUnitOf(ctx, tx, currency)
	if err != nil {
		return uuid.Nil, err
	}
	amountMinor, err := DecimalToMinor(row.AmountDecimal, mu)
	if err != nil {
		return uuid.Nil, common.Wrap(err)
	}

	delta := amountMinor
	fxApplied := false
	if currency != accCurrency {
		rate, err := nearestRate(ctx, tx, currency, accCurrency, row.OccurredAt)
		if err != nil {
			return uuid.Nil, err
		}
		toMU, err := minorUnitOf(ctx, tx, accCurrency)
		if err != nil {
			return uuid.Nil, err
		}
		delta, err = common.ConvertToMinor(amountMinor, rate, mu, toMU)
		if err != nil {
			return uuid.Nil, common.Wrap(err)
		}
		fxApplied = true
	}
	_ = fxApplied

	var txID uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO tx.transactions (org_id, account_id, kind, amount_minor, currency, amount_in_account_currency_minor, description, occurred_at, source, import_job_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'import', $9) RETURNING id`,
		orgID, accountID, kind, amountMinor, currency, delta, nullableString(row.Description), row.OccurredAt, jobID).Scan(&txID)
	if err != nil {
		return uuid.Nil, common.Wrap(err)
	}
	sign := int64(1)
	if kind == "expense" {
		sign = -1
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account.balance_ledger (account_id, delta_minor, reason, ref_id) VALUES ($1, $2, 'transaction', $3)`, accountID, sign*delta, txID); err != nil {
		return uuid.Nil, common.Wrap(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE account.accounts SET balance_minor = balance_minor + $2 WHERE id = $1`, accountID, sign*delta); err != nil {
		return uuid.Nil, common.Wrap(err)
	}
	return txID, nil
}

// nullableString maps an empty description to NULL so blank MEMO fields do
// not fill the column with empty strings.
func nullableString(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// encodeRaw serializes a parsed row for raw_rows.raw_data (JSONB).
func encodeRaw(row ParsedRow) string {
	data, _ := json.Marshal(row)
	return string(data)
}
