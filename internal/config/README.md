# config — Environment & Flags

Single `Config` struct loaded from env with flag overrides.

## Entry points (`config.go`)

`Load()` + `Validate()`. `Validate` requires `DATABASE_URL` and `JWT_PRIVATE_KEY_PATH`, and restricts `STORAGE_BACKEND ∈ {local, s3}`.

## Variables

| Field | Env | Default |
|---|---|---|
| `ListenAddr` | `LISTEN_ADDR` | `:9090` |
| `DatabaseURL` | `DATABASE_URL` | — (required) |
| `JWTPrivateKeyPath` | `JWT_PRIVATE_KEY_PATH` | — (required) |
| `JWTAccessTTL` | `JWT_ACCESS_TTL` | `15m` |
| `JWTRefreshTTL` | `JWT_REFRESH_TTL` | `720h` (30d) |
| `FXAPIURL` | `FX_API_URL` | `https://api.exchangerate.host` |
| `FXAPIKey` | `FX_API_KEY` | — |
| `StorageBackend` | `STORAGE_BACKEND` | `local` |
| `HasuraActionSecret` | `HASURA_ACTION_SECRET` | — (empty disables `X-Action-Secret` check) |

See `ARCHITECTURE.md` §9.1 for the full `.env` template (DB, Hasura, Go, FX, storage). `.env` is never committed.
