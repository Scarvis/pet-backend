# budgets — Budgets, Limits, Progress

Budget CRUD plus event-driven spend tracking. See `ARCHITECTURE.md` §8.4.

## Entry points (`handler.go`)

`Handler{pool, log}` + `NewHandler(pool, cfg, log)`.

Budget CRUD: `CreateBudget` / `ListBudgets` / `GetBudget` / `UpdateBudget` / `DeleteBudget` (cascade → limits → progress).
Limit CRUD: `CreateLimit` / `ListLimits` / `UpdateLimit` / `DeleteLimit` (cascade → progress).
Read: `GetProgress(limitID)` (read-only; progress is written by the event worker).

## Tables

`budget.budgets` (org-scoped), `budget.budget_limits` (`UNIQUE(budget_id, category_id)`, `alert_pct` nullable → `COALESCE(..., 80)`), `budget.budget_progress` (PK = `budget_limit_id`, written only from `progress.go`), `tx.categories` (org check).

## Rules (`progress.go`)

- Mutating handler ops run through `db.WithSerializable`; nil pointer = no change on update.
- `period ∈ {monthly, weekly, custom}`; `limit_minor > 0`; `1 ≤ alert_pct ≤ 100`.
- `PeriodBounds(period, startsOn, ref)`: monthly = calendar month clamped to `startsOn`; weekly = Monday 00:00 UTC, 7-day window (matches cron `0 0 * * 1`); custom = `[startsOn, 9999-12-31]` sentinel (`period_end NOT NULL`, never NULL).
- `HandleTransactionEvent`: exits early unless `kind == expense` with a category. Finds affected limits (exact category always; ancestors only when `include_subcategories`), recalculates `spent_minor` from scratch (`SUM(amount_minor)` over `[start, end)`, `deleted_at IS NULL`, subtree when include-sub), upserts progress.
- `ShouldAlert`: `spent*100 >= limit*alertPct` via `big.Int` (overflow-safe); `limit ≤ 0` never alerts. Alert path publishes `budget.alert` via `app.publish_event` and returns `[]Alert` for post-COMMIT notification.
- No FX conversion in spend sums; transfers excluded by the `kind='expense'` filter.

## Errors

`BadRequest` (empty name, bad period/limit/alertPct, unknown currency), `NotFound` (budget/limit/category/progress, incl. cross-org).

## Tests

`progress_test.go` — `PeriodBounds` (monthly/weekly-Monday/custom/bad period) and `ShouldAlert` boundaries. Pure functions only.
