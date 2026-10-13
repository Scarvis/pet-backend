// Daily category rollup rebuild (§8.7 rebuild_daily_summary): delete-then-insert
// over [from, ∞), excluding transfers. One DB transaction, UTC, under the same
// advisory lock as the event worker's analytics updates (the lock is taken by
// the Phase 7 dispatcher; this function documents the requirement and performs
// the transactional rebuild).
package analytics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RebuildDailySummary rebuilds analytics.daily_category_summary for every day
// >= from (§8.7). The DELETE removes days whose transactions were all deleted
// or moved; the INSERT re-aggregates live non-transfer transactions with a
// category. Transfers are excluded (neither income nor expense) and a NULL
// category never matches (§8.7 step 2). A category has a single kind, so a
// SUM never mixes income and expense.
func RebuildDailySummary(ctx context.Context, pool *pgxpool.Pool, from time.Time) error {
	day := from.UTC().Truncate(24 * time.Hour)
	return db.WithSerializable(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'`); err != nil {
			return common.Wrap(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM analytics.daily_category_summary WHERE day >= $1::date`, day); err != nil {
			return common.Wrap(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO analytics.daily_category_summary (org_id, category_id, day, total_minor, currency)
			SELECT org_id, category_id, occurred_at::date, SUM(amount_minor), currency
			FROM tx.transactions
			WHERE deleted_at IS NULL
			  AND kind <> 'transfer'
			  AND category_id IS NOT NULL
			  AND occurred_at >= $1::date
			GROUP BY 1, 2, 3, 5`, day); err != nil {
			return common.Wrap(err)
		}
		return nil
	})
}
