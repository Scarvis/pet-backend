// Budget progress recalculation (§8.4): upsert of budget_progress on
// transaction events, recursive category matching and alert publishing.
package budgets

import (
	"context"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Scarvis/pet-backend/internal/common"
)

// Alert is raised when spent_minor/limit_minor >= alert_pct/100 (§8.4 step 4).
// HandleTransactionEvent collects alerts inside the DB transaction; the caller
// sends the actual email/push only after COMMIT (§8.0 side-effects rule).
type Alert struct {
	BudgetLimitID uuid.UUID `json:"budget_limit_id"`
	BudgetID      uuid.UUID `json:"budget_id"`
	SpentMinor    int64     `json:"spent_minor"`
	LimitMinor    int64     `json:"limit_minor"`
	AlertPct      int       `json:"alert_pct"`
}

// PeriodBounds returns the current spending period for a budget. ref is usually
// now (UTC); the weekly period starts on Monday to match the
// rotate_budget_progress cron (`0 0 * * 1`, §6.6). Custom budgets have a single
// open-ended period: the end is the 9999-12-31 sentinel because
// budget_progress.period_end is NOT NULL (db_schema_0001.sql §7).
func PeriodBounds(period string, startsOn time.Time, ref time.Time) (time.Time, time.Time, error) {
	if err := validatePeriod(period); err != nil {
		return time.Time{}, time.Time{}, err
	}
	startsOn = startsOn.UTC().Truncate(24 * time.Hour)
	ref = ref.UTC()
	switch period {
	case "monthly":
		start := time.Date(ref.Year(), ref.Month(), 1, 0, 0, 0, 0, time.UTC)
		if start.Before(startsOn) {
			start = time.Date(startsOn.Year(), startsOn.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
		return start, start.AddDate(0, 1, 0), nil
	case "weekly":
		// Monday 00:00 of ref's week; Go Sunday==0 so remap to Monday==0.
		dow := (int(ref.Weekday()) + 6) % 7
		day := time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, time.UTC)
		start := day.AddDate(0, 0, -dow)
		if start.Before(startsOn) {
			start = startsOn
		}
		return start, start.AddDate(0, 0, 7), nil
	default: // custom
		return startsOn, time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), nil
	}
}

// ShouldAlert reports whether spent has reached the alert threshold. limit <= 0
// never alerts (defensive: the DB CHECK forbids it, but division needs a guard).
// The comparison is spent/limit >= alertPct/100 done in big integers so large
// minor-unit values cannot overflow int64.
func ShouldAlert(spentMinor, limitMinor int64, alertPct int) bool {
	if limitMinor <= 0 || alertPct <= 0 {
		return false
	}
	lhs := new(big.Int).Mul(big.NewInt(spentMinor), big.NewInt(100))
	rhs := new(big.Int).Mul(big.NewInt(limitMinor), big.NewInt(int64(alertPct)))
	return lhs.Cmp(rhs) >= 0
}

// affectedLimit is one budget_limit whose progress must be refreshed for a
// transaction in category txCategoryID.
type affectedLimit struct {
	Limit BudgetLimit
	// Budget carries the period definition needed for PeriodBounds.
	Period   string
	StartsOn time.Time
}

// findAffectedLimits returns every limit matching a transaction's category:
// an exact category match always counts; a limit on an ancestor category
// counts only when its include_subcategories is true. Ancestors are walked
// upward from the transaction category, which is equivalent to the downward
// subtree check in §8.4 step 2 but needs a single recursion.
func findAffectedLimits(ctx context.Context, tx pgx.Tx, orgID, txCategoryID uuid.UUID) ([]affectedLimit, error) {
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE ancestors AS (
			SELECT id, parent_id FROM tx.categories WHERE id = $1 AND org_id = $2
			UNION ALL
			SELECT c.id, c.parent_id FROM tx.categories c JOIN ancestors a ON c.id = a.parent_id
		)
		SELECT bl.id, bl.budget_id, bl.category_id, bl.limit_minor, bl.currency,
		       bl.include_subcategories, COALESCE(bl.alert_pct, 80), b.period, b.starts_on
		FROM budget.budget_limits bl
		JOIN budget.budgets b ON b.id = bl.budget_id
		JOIN ancestors a ON a.id = bl.category_id
		WHERE b.org_id = $2 AND (bl.category_id = $1 OR bl.include_subcategories)`,
		txCategoryID, orgID)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()
	var out []affectedLimit
	for rows.Next() {
		var l affectedLimit
		if err := rows.Scan(&l.Limit.ID, &l.Limit.BudgetID, &l.Limit.CategoryID,
			&l.Limit.LimitMinor, &l.Limit.Currency, &l.Limit.IncludeSubcategories,
			&l.Limit.AlertPct, &l.Period, &l.StartsOn); err != nil {
			return nil, common.Wrap(err)
		}
		out = append(out, l)
	}
	return out, common.Wrap(rows.Err())
}

// recalculateSpent sums expense transactions for a limit's category subtree
// over [periodStart, periodEnd) from scratch (§8.4 step 3). From-scratch
// recalculation is what makes .updated/.deleted correct without deltas.
// Transfers are excluded and NULL categories cannot match a limit's category.
// Amounts are summed raw in minor units without cross-currency conversion,
// matching the spec; limits are therefore most meaningful when the category's
// transactions share the limit currency.
func recalculateSpent(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, limit BudgetLimit, periodStart, periodEnd time.Time) (int64, error) {
	query := `
		WITH RECURSIVE subtree AS (
			SELECT id FROM tx.categories WHERE id = $1 AND org_id = $2
			UNION ALL
			SELECT c.id FROM tx.categories c JOIN subtree s ON c.parent_id = s.id
		)
		SELECT COALESCE(SUM(t.amount_minor), 0)
		FROM tx.transactions t
		WHERE t.org_id = $2
		  AND t.deleted_at IS NULL
		  AND t.kind = 'expense'
		  AND t.occurred_at >= $3
		  AND t.occurred_at < $4`
	// category predicate: exact match, or whole subtree when included.
	args := []any{limit.CategoryID, orgID, periodStart, periodEnd}
	if limit.IncludeSubcategories {
		query += ` AND t.category_id IN (SELECT id FROM subtree)`
	} else {
		query += ` AND t.category_id = $1`
	}
	var spent int64
	if err := tx.QueryRow(ctx, query, args...).Scan(&spent); err != nil {
		return 0, common.Wrap(err)
	}
	return spent, nil
}

// HandleTransactionEvent refreshes budget_progress for every limit affected by
// a transaction event (§8.4). kind/category are the transaction's current
// values; for .deleted the caller passes the pre-delete row so the
// recalculation still finds the right limits. Non-expense transactions exit
// early (§8.4 step 1). Progress rows are UPSERTed on PK budget_limit_id
// (§8.4 step 3); alerts are published via app.publish_event and returned for
// post-COMMIT notification. tx is the event worker's per-event transaction
// (§8.8), so all writes join it.
func HandleTransactionEvent(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, txCategoryID *uuid.UUID, kind string, ref time.Time) ([]Alert, error) {
	if kind != "expense" || txCategoryID == nil {
		return nil, nil
	}
	limits, err := findAffectedLimits(ctx, tx, orgID, *txCategoryID)
	if err != nil {
		return nil, err
	}
	var alerts []Alert
	for _, l := range limits {
		start, end, err := PeriodBounds(l.Period, l.StartsOn, ref)
		if err != nil {
			return nil, err
		}
		spent, err := recalculateSpent(ctx, tx, orgID, l.Limit, start, end)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO budget.budget_progress (budget_limit_id, spent_minor, period_start, period_end) VALUES ($1, $2, $3, $4) ON CONFLICT (budget_limit_id) DO UPDATE SET spent_minor = EXCLUDED.spent_minor, period_start = EXCLUDED.period_start, period_end = EXCLUDED.period_end`,
			l.Limit.ID, spent, start, end); err != nil {
			return nil, common.Wrap(err)
		}
		if ShouldAlert(spent, l.Limit.LimitMinor, l.Limit.AlertPct) {
			if _, err := tx.Exec(ctx, `SELECT app.publish_event('budget_limit', $1::text, 'budget.alert', jsonb_build_object('budget_id', $2::text, 'spent_minor', $3, 'limit_minor', $4))`,
				l.Limit.ID.String(), l.Limit.BudgetID.String(), spent, l.Limit.LimitMinor); err != nil {
				return nil, common.Wrap(err)
			}
			alerts = append(alerts, Alert{
				BudgetLimitID: l.Limit.ID,
				BudgetID:      l.Limit.BudgetID,
				SpentMinor:    spent,
				LimitMinor:    l.Limit.LimitMinor,
				AlertPct:      l.Limit.AlertPct,
			})
		}
	}
	return alerts, nil
}
