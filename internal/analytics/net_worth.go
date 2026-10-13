// Net-worth snapshots (§8.7 snapshot_net_worth): sum of active account balances
// converted into the organization's base currency at the latest FX rates.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/db"
)

// minorUnitOf returns fx.currencies.minor_unit for code, falling back to the
// compiled-in default when the row is missing (same fallback as
// common.MinorUnit).
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

// latestRate returns the most recent fx.rates row for base→quote (any as_of).
// Unlike transaction-time conversion (§8.2), snapshots always use the latest
// rates (§8.7).
func latestRate(ctx context.Context, tx pgx.Tx, base, quote string) (float64, error) {
	var rate float64
	err := tx.QueryRow(ctx, `SELECT rate FROM fx.rates WHERE base_code = $1 AND quote_code = $2 ORDER BY as_of DESC LIMIT 1`, base, quote).Scan(&rate)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, common.NotFound(fmt.Sprintf("no fx rate for %s→%s", base, quote))
	}
	if err != nil {
		return 0, common.Wrap(err)
	}
	return rate, nil
}

// SnapshotNetWorth sums all active (non-deleted) accounts of orgID, converting
// each balance into the org's base currency at the latest rates, and inserts
// the snapshot for asOf (date granularity). Same-currency balances need no
// rate; a missing rate for a foreign currency is a hard error so snapshots
// never silently drop accounts.
func SnapshotNetWorth(ctx context.Context, pool *pgxpool.Pool, orgID uuid.UUID, asOf time.Time) error {
	day := asOf.UTC().Truncate(24 * time.Hour)
	return db.WithSerializable(ctx, pool, func(tx pgx.Tx) error {
		var base string
		if err := tx.QueryRow(ctx, `SELECT base_currency FROM identity.organizations WHERE id = $1`, orgID).Scan(&base); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return common.Wrap(common.NotFound("organization not found"))
			}
			return common.Wrap(err)
		}
		baseMU, err := minorUnitOf(ctx, tx, base)
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `SELECT currency, SUM(balance_minor) FROM account.accounts WHERE org_id = $1 AND deleted_at IS NULL GROUP BY currency`, orgID)
		if err != nil {
			return common.Wrap(err)
		}
		defer rows.Close()

		var total int64
		for rows.Next() {
			var currency string
			var sum int64
			if err := rows.Scan(&currency, &sum); err != nil {
				return common.Wrap(err)
			}
			if currency == base {
				total += sum
				continue
			}
			rate, err := latestRate(ctx, tx, currency, base)
			if err != nil {
				return err
			}
			fromMU, err := minorUnitOf(ctx, tx, currency)
			if err != nil {
				return err
			}
			converted, err := common.ConvertToMinor(sum, rate, fromMU, baseMU)
			if err != nil {
				return common.Wrap(err)
			}
			total += converted
		}
		if err := rows.Err(); err != nil {
			return common.Wrap(err)
		}

		if _, err := tx.Exec(ctx, `INSERT INTO analytics.net_worth_snapshots (org_id, as_of, net_worth_minor, currency) VALUES ($1, $2, $3, $4) ON CONFLICT (org_id, as_of) DO UPDATE SET net_worth_minor = EXCLUDED.net_worth_minor, currency = EXCLUDED.currency`,
			orgID, day, total, base); err != nil {
			return common.Wrap(err)
		}
		return nil
	})
}

// SnapshotAllOrgs snapshots every organization. The cron job
// (snapshot_net_worth, §6.6) calls this; a single org failure aborts the run
// with an error so the miss is visible instead of silently skipped.
func SnapshotAllOrgs(ctx context.Context, pool *pgxpool.Pool, asOf time.Time) error {
	rows, err := pool.Query(ctx, `SELECT id FROM identity.organizations`)
	if err != nil {
		return common.Wrap(err)
	}
	var orgs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return common.Wrap(err)
		}
		orgs = append(orgs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return common.Wrap(err)
	}
	for _, orgID := range orgs {
		if err := SnapshotNetWorth(ctx, pool, orgID, asOf); err != nil {
			return err
		}
	}
	return nil
}
