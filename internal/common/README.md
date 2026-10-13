# common — Shared Helpers

Cross-cutting primitives every handler uses: identity, currency, money, errors, middleware.

## Files

| File | Exports |
|---|---|
| `context.go` | `WithUserID`, `CurrentUser(ctx)` (from `X-Hasura-User-Id`), `ActingOrg(ctx, pool)` (`SELECT org_id FROM identity.users`), `ValidCurrency(ctx, pool, code)` (`EXISTS fx.currencies`) |
| `money.go` | `Bigint` (string-backed scalar: `Int64`, `Marshal/UnmarshalJSON` string-only), `MinorUnits` map (JPY 0, BTC 8, ETH 18), `MinorUnit(code)` (default 2), `ToMinor`, `FromMinor`, `ConvertToMinor(from, rate, unitFrom, unitTo)` via `big.Float` — never floats |
| `errors.go` | `Error{Status, Code, Message}`, `New` / `BadRequest` (400) / `NotFound` (404) / `Conflict` (409), `ErrRetryable` + `IsRetryable` (→ HTTP 409), `Wrap`, `HTTPStatus`, `WriteError` (`{error:{code,message}}`), `WriteJSON` |
| `middleware.go` | `ConnProvider{Acquire}`, `Middleware{pool, secret, logger}`, `NewMiddleware` (empty secret disables the check for local dev), `Handler(next)` — verifies `X-Action-Secret` (401 on mismatch) + panic recovery |

## Conventions

- Money travels as decimal strings end-to-end (Hasura runs with `STRINGIFY_NUMERIC_TYPES=true`); parse with `Bigint.Int64`.
- `SET LOCAL app.via_action = 'on'` is set inside each mutating handler's own tx — a middleware connection cannot provide it.
- Cross-org access is reported as `NotFound`, never forbidden.

## Tests

`money_test.go` — minor-unit conversion cases.
