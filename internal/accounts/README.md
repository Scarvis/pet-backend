# accounts — Accounts

CRUD-lite for `account.accounts`. Balance is never set directly — only via transactions, transfers, and imports.

## Entry points (`handler.go`)

`Handler{pool, log}` + `NewHandler(pool, cfg, log)`.

| Method | Route | Notes |
|---|---|---|
| `Create(ctx, name, typ, currency)` | `POST /actions/account/create` | Validates type + currency, `balance_minor` from DB default |
| `List(ctx)` / `Get(ctx, id)` | Hasura direct (no Action) | Org-scoped reads |
| `Archive(ctx, id)` / `Unarchive(ctx, id)` | Hasura direct (no Action) | Soft delete via `deleted_at` |

## Tables

`account.accounts` only. Org scoping via `common.ActingOrg`; owner from `common.CurrentUser`.

## Rules

- Mutations run through `db.WithSerializable`.
- `type ∈ {cash, card, deposit, debt}`; currency must exist in `fx.currencies` (`common.ValidCurrency`).
- `Archive`/`Unarchive` are idempotent no-ops when already in the target state.
- Cross-org access returns `NotFound`, not forbidden.

## Errors

`BadRequest("account type must be …")`, `BadRequest("unknown currency %q")`, `NotFound("account not found")`.

## Tests

None — handler needs a DB; no pure functions to unit-test in isolation.
