# analytics — Read Models

Aggregations backing dashboards: daily per-category totals and net-worth snapshots. See `ARCHITECTURE.md` §8.7.

## Entry points

`Handler{pool, log}` + `NewHandler(pool, cfg, log)`.

| Method | Notes |
|---|---|
| `Rebuild(ctx, from)` | `POST /actions/analytics/rebuild` → `RebuildDailySummary` |
| `SnapshotNetWorth(ctx, asOf)` | Session-org snapshot (resolves org via `common.ActingOrg`) |
| `SnapshotOrg(ctx, orgID, asOf)` | Explicit-org variant for cron |
| `RebuildDailySummary(ctx, pool, from)` (`daily_summary.go`) | Delete-then-insert over `[from, ∞)` |
| `SnapshotNetWorth(ctx, pool, orgID, asOf)` / `SnapshotAllOrgs` (`net_worth.go`) | Per-org insert / cron loop over all orgs |

Crons: `rebuild_daily_summary` (`30 0 * * *`), `snapshot_net_worth` (`0 1 * * *`).

## Tables

- `analytics.daily_category_summary` (DELETE `day >= from`, then `INSERT … SELECT`)
- `tx.transactions` (read: `deleted_at IS NULL AND kind <> 'transfer' AND category_id IS NOT NULL AND occurred_at >= from`, grouped by org/category/day/currency)
- `analytics.net_worth_snapshots` (`INSERT … ON CONFLICT(org_id, as_of) DO UPDATE`)
- `account.accounts` (active balances grouped by currency), `identity.organizations` (`base_currency`), `fx.rates` (latest rate), `fx.currencies` (`minor_unit`)

## Rules

- All in `db.WithSerializable` with `SET LOCAL TIME ZONE 'UTC'`; day = `occurred_at::date`.
- Transfers excluded (neither income nor expense); a category has a single kind so `SUM` never mixes them. The DELETE step removes days whose transactions were all deleted or moved.
- Net worth: same-currency balances added directly; foreign balances converted with the **latest** rate (not the transaction-time rate) via `common.ConvertToMinor`. Missing rate is a hard `NotFound` — never silently dropped. `asOf` truncated to UTC midnight.
- `SnapshotAllOrgs` aborts on the first failing org (a missing snapshot is visible; a partial one is not). Shares the advisory lock with the event worker's analytics updates.

## Errors

`Wrap` for DB failures; `NotFound` for missing org/rate.

## Tests

None — all functions need a DB.
