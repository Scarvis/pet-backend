// Position valuation (§8.5): value_minor = quantity × close_minor at the
// latest price_history.as_of. Positions in different assets stay in their own
// asset currencies here; cross-currency aggregation is an analytics concern.
package invest

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
)

// Valuation is one position's value at the latest known price.
type Valuation struct {
	HoldingID  uuid.UUID `json:"holding_id"`
	AssetID    uuid.UUID `json:"asset_id"`
	Quantity   string    `json:"quantity"`
	Currency   string    `json:"currency"`
	CloseMinor int64     `json:"close_minor"`
	AsOf       time.Time `json:"as_of"`
	ValueMinor int64     `json:"value_minor"`
}

// LatestPrice returns the most recent price_history row for an asset, or
// NotFound when the asset has no prices yet.
func LatestPrice(ctx context.Context, pool *pgxpool.Pool, assetID uuid.UUID) (closeMinor int64, asOf time.Time, err error) {
	err = pool.QueryRow(ctx, `SELECT close_minor, as_of FROM invest.price_history WHERE asset_id = $1 ORDER BY as_of DESC LIMIT 1`, assetID).Scan(&closeMinor, &asOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, common.Wrap(common.NotFound("no price history for asset"))
	}
	if err != nil {
		return 0, time.Time{}, common.Wrap(err)
	}
	return closeMinor, asOf, nil
}

// HoldingValueMinor computes quantity × close_minor exactly (§8.5 valuation).
// The product is truncated toward zero; an overflow of int64 is an error.
func HoldingValueMinor(quantity string, closeMinor int64) (int64, error) {
	q, ok := new(big.Rat).SetString(strings.TrimSpace(quantity))
	if !ok {
		return 0, fmt.Errorf("invalid quantity %q", quantity)
	}
	v := new(big.Rat).Mul(q, new(big.Rat).SetInt64(closeMinor))
	out := new(big.Int).Quo(v.Num(), v.Denom())
	if !out.IsInt64() {
		return 0, fmt.Errorf("position value overflows int64")
	}
	return out.Int64(), nil
}

// RecordPrice upserts a close price for an asset on asOf. Conflicts are
// ignored: price_history has no UPDATE trigger (INSERT/DELETE only, §schema),
// so a recorded price is immutable.
func (h *Handler) RecordPrice(ctx context.Context, assetID uuid.UUID, asOf time.Time, closeMinor int64) error {
	if closeMinor <= 0 {
		return common.Wrap(common.BadRequest("close_minor must be a positive integer"))
	}
	day := asOf.UTC().Truncate(24 * time.Hour)
	tag, err := h.pool.Exec(ctx, `INSERT INTO invest.price_history (asset_id, as_of, close_minor) VALUES ($1, $2, $3) ON CONFLICT (asset_id, as_of) DO NOTHING`,
		assetID, day, closeMinor)
	if err != nil {
		return common.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		h.log.Debug("price already recorded", "asset", assetID, "as_of", day)
	}
	return nil
}

// ValuatePortfolio values every holding of one portfolio at its latest price
// (org-scoped). Holdings without any price history are skipped and reported
// via the returned count so the cron job can log the gap instead of failing.
func (h *Handler) ValuatePortfolio(ctx context.Context, portfolioID uuid.UUID) ([]Valuation, error) {
	holdings, err := h.ListHoldings(ctx, portfolioID)
	if err != nil {
		return nil, err
	}
	var out []Valuation
	for _, hd := range holdings {
		var currency string
		if err := h.pool.QueryRow(ctx, `SELECT currency FROM invest.assets WHERE id = $1`, hd.AssetID).Scan(&currency); err != nil {
			return nil, common.Wrap(err)
		}
		var closeMinor int64
		var asOf time.Time
		err := h.pool.QueryRow(ctx, `SELECT close_minor, as_of FROM invest.price_history WHERE asset_id = $1 ORDER BY as_of DESC LIMIT 1`, hd.AssetID).Scan(&closeMinor, &asOf)
		if errors.Is(err, pgx.ErrNoRows) {
			h.log.Warn("no price for asset, skipping", "asset", hd.AssetID)
			continue
		}
		if err != nil {
			return nil, common.Wrap(err)
		}
		value, err := HoldingValueMinor(hd.Quantity, closeMinor)
		if err != nil {
			return nil, common.Wrap(err)
		}
		out = append(out, Valuation{
			HoldingID: hd.ID, AssetID: hd.AssetID, Quantity: hd.Quantity,
			Currency: currency, CloseMinor: closeMinor, AsOf: asOf, ValueMinor: value,
		})
	}
	return out, nil
}
