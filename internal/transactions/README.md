# transactions — Transactions, Transfers, Balances

Single- and double-leg money movement with FX conversion and ledger-backed reversals. See `ARCHITECTURE.md` §8.2–§8.3.

## Entry points

`Handler{pool, log}` + `NewHandler(pool, cfg, log)`.

| Method | File | Route |
|---|---|---|
| `CreateTransaction(ctx, in)` | `handler.go` | `POST /actions/tx/create` |
| `CreateTransfer(ctx, in)` | `transfer.go` | `POST /actions/tx/transfer` |
| `UpdateTransaction(ctx, in)` | `handler.go` | `POST /actions/tx/update` |
| `DeleteTransaction(ctx, in)` | `handler.go` | `POST /actions/tx/delete` |

Helpers in `balance.go`: `nearestRate`, `convertToAccountMinor`, `applyLedgerAndBalance`, `reverseEffect`. Pure helpers: `validateKind`, `sameDay`.

## Tables

- `tx.transactions` (insert `source=manual`; `FOR UPDATE` lock on update/delete; soft delete via `deleted_at`)
- `account.accounts` (locked ascending-id for deadlock protection; `balance_minor += delta`)
- `account.balance_ledger` (`reason ∈ {transaction, transfer, adjustment}`, `ref_id = tx.id`)
- `tx.categories` (org check), `fx.rates` (nearest `as_of <= occurred_at::date`)

## Rules

- All mutations through `db.WithSerializable`; update/delete set `SET LOCAL app.via_action = 'on'` inside their own tx (satisfies the guard trigger).
- Money is integer minor units (`common.Bigint` as JSON string); `amount_minor > 0`; `kind ∈ {income, expense}` for create (`transfer` only via transfer endpoint).
- FX: same currency → `fx_applied=false`; else nearest historical rate + `common.ConvertToMinor` (minor-unit aware, never floats).
- Reversals sum `balance_ledger` by `ref_id` — never recompute from current FX rates.
- Update rejects `transfer` kind (transfers are immutable); no-op when only description/category changes on the same day.
- Transfer writes two ledger legs (source `−`, target `+` converted), `category_id=NULL`, same-account rejected.

## Errors

`BadRequest` for kind/amount/currency/FX/transfer-shape violations; `NotFound` for account/category/transaction (cross-org masked) and missing FX rate (`no fx rate for B→Q on or before …`).

## Tests

`handler_test.go` — `TestValidateKind`, `TestSameDay` (pure functions only; handlers need a DB).
