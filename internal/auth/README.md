# auth — Identity & JWT

Registration, login, and refresh-token rotation. Issues Hasura-compatible RS256 JWTs.

## Entry points (`handler.go`)

`Handler{pool, cfg, log}` + `NewHandler(pool, cfg, log)` shape.

| Method | Route | Notes |
|---|---|---|
| `Register(ctx, email, password)` | `POST /actions/auth/register` | Creates org + user + role + template category copy in one `SERIALIZABLE` tx |
| `Login(ctx, email, password)` | `POST /actions/auth/login` | bcrypt check, inserts refresh token |
| `RefreshToken(ctx, token)` | `POST /actions/auth/refresh` | Single-use rotation: revoke old, issue new pair |

Cron `POST /cron/cleanup_tokens` deletes expired `identity.refresh_tokens` (no handler method, inline SQL in `cmd/server`).

## Tables

`identity.organizations` (plan=`free`, `base_currency=USD` fixed), `identity.users`, `identity.roles` (`owner` lookup), `identity.user_roles`, `tx.system_categories` (global template) → `tx.categories` (per-org copy), `identity.refresh_tokens`.

## Rules

- `Register` runs through `db.WithSerializable`; JWT issue happens after `COMMIT`.
- Every new user becomes `owner` of a new org.
- Refresh tokens: 32 random bytes hex-encoded, stored as SHA256 hex, 30d TTL, `revoked_at IS NULL` required.
- JWT claims: `sub` + `https://hasura.io/jwt/claims` (`allowed-roles`, `default-role`, `org-id`, `user-id`), `iat`/`exp`. RSA key lazy-loaded from `JWT_PRIVATE_KEY_PATH`.
- Passwords: bcrypt cost 12 (`password.go`: `Hash` / `Compare`).

## Helpers (`jwt.go`)

`AuthClaims.Issue(priv)`, `Verify(token, pub)`, `loadPrivateKey[path|Bytes]` (PKCS#1 + PKCS#8).

## Errors

`NotFound("invalid credentials")` (unknown email / bad refresh token), `BadRequest("invalid credentials" | "refresh token expired")`, `common.Wrap` for DB/key failures.

## Tests

`jwt_test.go` — hash/compare round-trip, issue/verify round-trip, tamper rejection, PKCS#8 key load. Pure functions only; no live DB.
