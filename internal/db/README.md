# db — Pool & Transactions

PostgreSQL wiring: connection pool and the serializable-retry helper. See `ARCHITECTURE.md` §8.0.

## Entry points

| Function | File | Notes |
|---|---|---|
| `NewPool(ctx, cfg)` | `pool.go` | 20 max / 2 min conns, 1h max lifetime, 10s health ping |
| `WithSerializable(ctx, pool, fn(pgx.Tx))` | `tx.go` | `SERIALIZABLE` tx retried ×5 on `40001` (serialization failure) / `40P01` (deadlock) with jittered backoff (~5/10/20ms); exhausted attempts surface as retryable (HTTP 409) |

## Rules

- The closure must contain **only DB work** and be safe to re-run (validation + reads included — the whole closure retries).
- No email/push/HTTP inside the closure; side effects go after `COMMIT`.
- Trade execution (`invest`) uses a `FOR UPDATE` row lock instead and needs no `SERIALIZABLE`.
