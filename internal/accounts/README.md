# accounts — Accounts

CRUD-lite for `account.accounts`. Balance is never set directly — only via transactions, transfers, and imports.

## Entry points (`handler.go`)

`Handler{pool, log}` + `NewHandler(pool, cfg, log)`.

| Method | Route | Notes |
|---|---|---|
| `Create(ctx, name, typ, currency, initialAmount, isHidden)` | `POST /actions/account/create` | Validates type + currency, opens with `balance_minor = initial_amount` (+ opening `balance_ledger` row when nonzero); `is_hidden` visibility flag (default false) |
| `List(ctx)` / `Get(ctx, id)` | Hasura direct (no Action) | Org-scoped reads |
| `Archive(ctx, id)` / `Unarchive(ctx, id)` | Hasura direct (no Action) | Soft delete via `deleted_at` |

## Tables

`account.accounts` (+ `account.accounts_type` catalog: `type` is a FK to it) and `account.balance_ledger` (opening row on create). Org scoping via `common.ActingOrg`; owner from `common.CurrentUser`.

## Rules

- Mutations run through `db.WithSerializable`.
- `type ∈ {debit card, cash, deposit, broker, credit card, credit, debt, crypto, saving}` (catalog `account.accounts_type`, migration 000008; the DB FK enforces it, Go fails fast); currency must exist in `fx.currencies` (`common.ValidCurrency`).
- `initial_amount` is the opening balance in minor units, fixed at creation; `balance_minor` starts equal to it and moves with transactions. `initial_amount` is immutable via Hasura (not in update permissions).
- `Archive`/`Unarchive` are idempotent no-ops when already in the target state.
- `is_hidden` is a client-side visibility flag, independent from archiving: hidden accounts stay active and count toward balances/net worth. Toggled directly via Hasura (`is_hidden` is in insert/update permissions).
- Cross-org access returns `NotFound`, not forbidden.

## Errors

`BadRequest("account type must be …")`, `BadRequest("unknown currency %q")`, `NotFound("account not found")`.

## Tests

None — handler needs a DB; no pure functions to unit-test in isolation.
