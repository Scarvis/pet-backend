// Package transactions implements create/update/delete/transfer of transactions,
// their balance and ledger effects and FX conversion (§7.1, §8.2–§8.3). All money
// is handled in integer minor units; conversions use the nearest historical rate.
package transactions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Scarvis/pet-backend/internal/common"
)

// Rate is one fx.rates row used for FX conversion (§8.2 step 3).
type Rate struct {
	BaseCode  string // rate expressed as base→quote (see §schema rates table)
	QuoteCode string
	Rate      float64 // NUMERIC(20,10); float64 is sufficient precision for a daily rate
}

// nearestRate returns the most recent rate where base→quote on or before asOf, or
// NotFound when no such rate exists. Rates are unique per (base, quote, as_of) and
// ordered by as_of descending (§8.2 step 3). The comparison is date-granular: rates
// are stored per calendar day.
func (h *Handler) nearestRate(ctx context.Context, base, quote string, asOf time.Time) (*Rate, error) {
	var rate float64
	err := h.pool.QueryRow(ctx, `SELECT rate FROM fx.rates WHERE base_code = $1 AND quote_code = $2 AND as_of <= $3 ORDER BY as_of DESC LIMIT 1`, base, quote, asOf).Scan(&rate)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, common.NotFound(fmt.Sprintf("no fx rate for %s→%s on or before %s", base, quote, asOf.Format("2006-01-02")))
	}
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &Rate{BaseCode: base, QuoteCode: quote, Rate: rate}, nil
}

// convertToAccountMinor converts amountMinor (expressed in txCurrency) into the
// account's currency minor units using the nearest historical rate. When the two
// currencies are equal it returns the amount unchanged with fxApplied=false (§8.2).
func (h *Handler) convertToAccountMinor(ctx context.Context, amountMinor int64, txCurrency, accCurrency string, occurredAt time.Time) (int64, bool, error) {
	if txCurrency == accCurrency {
		return amountMinor, false, nil
	}
	rate, err := h.nearestRate(ctx, txCurrency, accCurrency, occurredAt)
	if err != nil {
		return 0, false, err
	}
	muFrom := common.MinorUnit(txCurrency)
	muTo := common.MinorUnit(accCurrency)
	converted, err := common.ConvertToMinor(amountMinor, rate.Rate, muFrom, muTo)
	if err != nil {
		return 0, false, common.Wrap(err)
	}
	return converted, true, nil
}

// applyLedgerAndBalance records a movement on accountID and adjusts its balance by
// delta. It is used both to apply a transaction's effect (reason='transaction'/'transfer')
// and to reverse it (reason='adjustment'). Every row carries the owning transaction's
// id in ref_id so reversals can re-sum the effect from the ledger instead of current
// FX rates (§8.2 step 4); reversal rows share the same ref_id so repeated
// reverse/apply cycles keep summing to the latest effect. The ledger row is written
// before the balance update so the append-only history reflects the change first
// (§8.2 step 5). Accounts that are archived (deleted_at set) are left untouched.
func applyLedgerAndBalance(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, delta int64, reason string, refID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `INSERT INTO account.balance_ledger (account_id, delta_minor, reason, ref_id) VALUES ($1, $2, $3, $4)`, accountID, delta, reason, refID); err != nil {
		return common.Wrap(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE account.accounts SET balance_minor = balance_minor + $2 WHERE id = $1 AND deleted_at IS NULL`, accountID, delta); err != nil {
		return common.Wrap(err)
	}
	return nil
}

// reverseEffect undoes the original effect of a transaction: it sums the net ledger
// movement per account for refID and applies the opposite sign to each affected
// account. It recomputes nothing from current FX rates (§8.2 step 4). The returned
// map holds the original (pre-reversal) delta per account, which callers may return.
func reverseEffect(ctx context.Context, tx pgx.Tx, refID uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := tx.Query(ctx, `SELECT account_id, SUM(delta_minor) FROM account.balance_ledger WHERE ref_id = $1 GROUP BY account_id`, refID)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()

	net := map[uuid.UUID]int64{}
	for rows.Next() {
		var accID uuid.UUID
		var sum int64
		if err := rows.Scan(&accID, &sum); err != nil {
			return nil, common.Wrap(err)
		}
		net[accID] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, common.Wrap(err)
	}

	reversal := map[uuid.UUID]int64{}
	for accID, sum := range net {
		d := -sum
		if err := applyLedgerAndBalance(ctx, tx, accID, d, "adjustment", refID); err != nil {
			return nil, err
		}
		reversal[accID] = d
	}
	return reversal, nil
}
