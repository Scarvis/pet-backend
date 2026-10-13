# invest — Portfolios, Trades, Valuation

Position bookkeeping with weighted-average cost and latest-price valuation. See `ARCHITECTURE.md` §8.5.

## Entry points

`Handler{pool, log}` + `NewHandler(pool, cfg, log)`.

| Method | File | Route |
|---|---|---|
| `CreatePortfolio` / `ListPortfolios` / `DeletePortfolio` | `handler.go` | Direct via Hasura (no Action) |
| `ListHoldings` / `GetHolding` | `handler.go` | Direct via Hasura (read-only) |
| `ExecuteTrade(ctx, in)` | `trade.go` | `POST /actions/invest/trade` |
| `LatestPrice` / `RecordPrice` / `ValuatePortfolio` / `HoldingValueMinor` | `valuation.go` | Cron / worker use |

`CreateTradeInput{portfolioID, assetID, side, quantity (decimal string), priceMinor, executedAt}` → `TradeOutput{holding_id, quantity, avg_cost_minor}`.

## Tables

`invest.portfolios` (org-scoped), `invest.assets` (global catalog), `invest.holdings` (`UNIQUE(portfolio_id, asset_id)`, `quantity NUMERIC(28,10)`), `invest.trades` (FK → holding), `invest.price_history(asset_id, as_of, close_minor)`.

## Rules

- `ExecuteTrade` uses a plain tx + `SELECT … FOR UPDATE` on the holding (row lock serializes; no `WithSerializable` needed).
- Quantity parsed as `big.Rat`, must be `> 0`; formatted with `FloatString(10)`, trimmed, never scientific.
- Buy: find-or-create holding (`ON CONFLICT DO NOTHING`), `newAvg = (oldQty·oldAvg + qty·price) / newQty` truncated; sell: requires `oldQty ≥ qty`, average unchanged; oversell rejected.
- Valuation: `value = quantity × latest close_minor` via exact `big.Rat`, truncated, `int64` overflow is an error. No cross-currency aggregation.
- `RecordPrice`: `close > 0`, day-truncated UTC, `ON CONFLICT(asset_id, as_of) DO NOTHING` (prices immutable). Holdings without a price are skipped with a warning, not a failure.

## Errors

`BadRequest` (empty name, bad side/quantity/price, oversell, bad close), `NotFound` (portfolio/asset/holding, missing price history; cross-org masked), `Wrap` for DB/scan failures.

## Tests

`trade_test.go` — average-cost math, fractional quantities, quantity parse/format rejection, `HoldingValueMinor`. Pure functions only.
