// Trade execution (§8.5): find-or-create the position, lock it with FOR UPDATE,
// apply the buy/sell, insert the trade and update the holding — all in one
// plain transaction. Per §8.0 the FOR UPDATE row lock serializes concurrent
// trades, so no SERIALIZABLE retry is needed here.
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

	"github.com/Scarvis/pet-backend/internal/common"
)

// sides are the CHECK-constraint values for invest.trades.side (§3.2).
var tradeSides = map[string]bool{"buy": true, "sell": true}

// CreateTradeInput mirrors the GraphQL input (§6.7). Quantity is NUMERIC,
// carried as a decimal string; PriceMinor is minor units in the asset's
// currency.
type CreateTradeInput struct {
	PortfolioID uuid.UUID
	AssetID     uuid.UUID
	Side        string // buy | sell
	Quantity    string // decimal, > 0
	PriceMinor  common.Bigint
	ExecutedAt  time.Time
}

// TradeOutput is returned by createTrade (§6.7): the updated position.
type TradeOutput struct {
	HoldingID    uuid.UUID `json:"holding_id"`
	Quantity     string    `json:"quantity"`
	AvgCostMinor int64     `json:"avg_cost_minor"`
}

// parseQuantity parses a decimal quantity, rejecting non-positive values.
func parseQuantity(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	q, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, common.BadRequest(fmt.Sprintf("invalid quantity %q", s))
	}
	if q.Sign() <= 0 {
		return nil, common.BadRequest("quantity must be greater than zero")
	}
	return q, nil
}

// formatQuantity renders a quantity for NUMERIC(28,10): at most 10 fractional
// digits, no trailing zeros, never scientific notation.
func formatQuantity(q *big.Rat) string {
	s := q.FloatString(10)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// applyBuy returns the new quantity and average cost after a buy:
// new_qty = old_qty + qty,
// new_avg = (old_qty*old_avg + qty*price) / new_qty, truncated (§8.5 step 4).
func applyBuy(oldQty, qty *big.Rat, oldAvg, price int64) (newQty *big.Rat, newAvg int64) {
	newQty = new(big.Rat).Add(oldQty, qty)
	total := new(big.Rat).Add(
		new(big.Rat).Mul(oldQty, new(big.Rat).SetInt64(oldAvg)),
		new(big.Rat).Mul(qty, new(big.Rat).SetInt64(price)),
	)
	avg := new(big.Rat).Quo(total, newQty)
	newAvg = new(big.Int).Quo(avg.Num(), avg.Denom()).Int64()
	return newQty, newAvg
}

// ExecuteTrade runs one trade against a position (§8.5). The portfolio must
// belong to the acting org; assets are a global catalog. A sell exceeding the
// position is rejected. Returns the updated holding.
func (h *Handler) ExecuteTrade(ctx context.Context, input CreateTradeInput) (*TradeOutput, error) {
	if !tradeSides[input.Side] {
		return nil, common.Wrap(common.BadRequest("side must be one of buy, sell"))
	}
	qty, err := parseQuantity(input.Quantity)
	if err != nil {
		return nil, common.Wrap(err)
	}
	price, err := input.PriceMinor.Int64()
	if err != nil || price <= 0 {
		return nil, common.Wrap(common.BadRequest("price_minor must be a positive integer"))
	}
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer tx.Rollback(ctx)

	var portfolioOrg uuid.UUID
	err = tx.QueryRow(ctx, `SELECT org_id FROM invest.portfolios WHERE id = $1`, input.PortfolioID).Scan(&portfolioOrg)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, common.Wrap(common.NotFound("portfolio not found"))
	}
	if err != nil {
		return nil, common.Wrap(err)
	}
	if portfolioOrg != orgID {
		return nil, common.Wrap(common.NotFound("portfolio not found"))
	}
	var assetID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM invest.assets WHERE id = $1`, input.AssetID).Scan(&assetID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, common.Wrap(common.NotFound("asset not found"))
		}
		return nil, common.Wrap(err)
	}

	// Find or create the position (§8.5 steps 2–3), then lock it.
	if _, err := tx.Exec(ctx, `INSERT INTO invest.holdings (portfolio_id, asset_id, quantity, avg_cost_minor) VALUES ($1, $2, 0, 0) ON CONFLICT (portfolio_id, asset_id) DO NOTHING`,
		input.PortfolioID, input.AssetID); err != nil {
		return nil, common.Wrap(err)
	}
	var holdingID uuid.UUID
	var oldQtyStr string
	var oldAvg int64
	err = tx.QueryRow(ctx, `SELECT id, quantity::text, avg_cost_minor FROM invest.holdings WHERE portfolio_id = $1 AND asset_id = $2 FOR UPDATE`,
		input.PortfolioID, input.AssetID).Scan(&holdingID, &oldQtyStr, &oldAvg)
	if err != nil {
		return nil, common.Wrap(err)
	}
	oldQty, ok := new(big.Rat).SetString(strings.TrimSpace(oldQtyStr))
	if !ok {
		return nil, common.Wrap(fmt.Errorf("invalid stored quantity %q", oldQtyStr))
	}

	var newQty *big.Rat
	newAvg := oldAvg
	switch input.Side {
	case "buy":
		newQty, newAvg = applyBuy(oldQty, qty, oldAvg, price)
	case "sell":
		if oldQty.Cmp(qty) < 0 {
			return nil, common.Wrap(common.BadRequest("insufficient quantity for sell"))
		}
		newQty = new(big.Rat).Sub(oldQty, qty)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO invest.trades (holding_id, side, quantity, price_minor, executed_at) VALUES ($1, $2, $3, $4, $5)`,
		holdingID, input.Side, formatQuantity(qty), price, input.ExecutedAt); err != nil {
		return nil, common.Wrap(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE invest.holdings SET quantity = $2 WHERE id = $1`, holdingID, formatQuantity(newQty)); err != nil {
		return nil, common.Wrap(err)
	}
	if input.Side == "buy" {
		if _, err := tx.Exec(ctx, `UPDATE invest.holdings SET avg_cost_minor = $2 WHERE id = $1`, holdingID, newAvg); err != nil {
			return nil, common.Wrap(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, common.Wrap(err)
	}

	out := &TradeOutput{HoldingID: holdingID, Quantity: formatQuantity(newQty), AvgCostMinor: newAvg}
	h.log.Info("trade executed", "holding", holdingID, "side", input.Side)
	return out, nil
}
