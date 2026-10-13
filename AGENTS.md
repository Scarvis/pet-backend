# AGENTS.md — pet-backend

Go business-logic service for a Hasura + PostgreSQL finance app. Module
`github.com/Scarvis/pet-backend`, Go 1.23. Spec is `ARCHITECTURE.md`
(§8 flows); `PLAN.md` tracks implementation phases. `cmd/server/main.go` is
still a stub (Phase 8) — each domain package is currently standalone.

## Commands (PowerShell — `&&` does NOT chain, use `;`)

```powershell
go build ./...; go vet ./...; gofmt -l cmd internal graphql; go test ./internal/<pkg>/
```

- Verify in this order: build → vet → gofmt (output must be empty) → test.
- Single package: `go test ./internal/budgets/`; full suite with race detector:
  `go test -race -cover ./...` (same as `make test`).
- `gofmt -l` flags files under `.agents/skills/` — ignore those, only
  `cmd`/`internal`/`graphql` must be clean.
- `make lint` and `make migrate-*` do not work here: no golangci config, no
  `migrations/` dir (Phase 9 open item), no CI.

## Schema source of truth

- The real DDL is the **sibling repo** `../db` (`db_schema_0001/0002/0003.sql`,
  run in order), reached via `db.lnk` (a gitignored Windows shortcut).
  `SCHEMAS.md` (untracked) is only a summary.
- When prose (`ARCHITECTURE.md`/`SCHEMAS.md`) conflicts with `../db/*.sql`,
  trust the SQL. Known cases: `budget_progress.period_end` is `NOT NULL`
  (custom periods use a `9999-12-31` sentinel, never NULL);
  `budget_limits.alert_pct` is nullable (read with `COALESCE(..., 80)`);
  `app.publish_event()` returns `void`.
- There is **no live database** in this repo and no integration-test setup —
  handlers compile/vet cleanly but cannot run. Write unit tests for pure
  functions only (see `internal/budgets/progress_test.go`).

## Conventions (all from ARCHITECTURE.md §8)

- Every mutating handler runs through `db.WithSerializable` (retries the whole
  closure on SQLSTATE `40001`/`40P01`); closures must contain only DB work.
  No email/push/HTTP inside — side effects go after COMMIT.
- Balance reversals (update/delete) sum `account.balance_ledger` by `ref_id`;
  never recompute from current FX rates. Lock multiple accounts in ascending
  id order. Transfers are two ledger legs, rejected by update.
- Money is integer minor units end-to-end (`common.Bigint` travels as a JSON
  string); FX math via `common.ConvertToMinor` with each currency's
  `minor_unit`, never floats.
- Use the shared helpers: `common.ActingOrg` for org scoping,
  `common.ValidCurrency` before inserts, `common.Wrap`/`BadRequest`/`NotFound`
  for errors. Handlers follow the `Handler{pool, log}` +
  `NewHandler(pool, cfg, log)` shape.
- Events: `SELECT app.publish_event(type, id, event, payload)` joins the
  worker's per-event transaction; notifications to users are collected and
  sent post-COMMIT (at-least-once).
- Weekly budget periods start Monday (matches the cron `0 0 * * 1`).
- Useful skill: `golang-how-to` orchestrates the Go skill set for
  implementation questions.
