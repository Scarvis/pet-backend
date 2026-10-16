# AGENTS.md — pet-backend

Go business-logic service for a Hasura + PostgreSQL finance app. Module
`github.com/Scarvis/pet-backend` (`go 1.26.0` directive). Spec is
`ARCHITECTURE.md` (§8 flows); `PLAN.md` phases 0–9 are all done.
Entrypoint is `cmd/server` (`package main`: `main.go` wiring + LISTEN worker,
`routes.go` Action/Event/Cron routes, `types.go` Hasura envelope decoding);
domain logic lives in `internal/<domain>`; `internal/observability` holds the
§10 Prometheus metrics; `graphql/actions.graphql` is the Hasura contract
(Action/Event/Cron + string-backed `Bigint` scalar).

## Commands (PowerShell — `&&` does NOT chain, use `;`)

```powershell
go build ./...; go vet ./...; gofmt -l cmd internal graphql; go test ./internal/<pkg>/
```

- Verify in this order: build → vet → gofmt (output must be empty) → test.
- Single package: `go test ./internal/budgets/`; full suite: `make test`
  (`go test -race -cover ./...`), but `-race` needs a cgo toolchain and fails
  on machines without one — plain `go test ./...` always works.
- `gofmt -l` flags files under `.agents/skills/` — ignore those, only
  `cmd`/`internal`/`graphql` must be clean.
- No `.github/`, no CI, no golangci config, no `migrations/` dir:
  `make lint` and `make migrate-*` do not work here.

## Schema source of truth

- The real DDL is the **sibling repo** `../db` (`db_schema_0001/0002/0003.sql`
  plus `db_schema_0005.sql`, run in numeric order), reached via `db.lnk`
  (gitignored). `SCHEMAS.md` (untracked) is only a summary.
- When prose (`ARCHITECTURE.md`/`SCHEMAS.md`/`PLAN.md`) conflicts with
  `../db/*.sql`, trust the SQL. Known cases: `budget_progress.period_end` is
  `NOT NULL` (custom periods use a `9999-12-31` sentinel, never NULL);
  `budget_limits.alert_pct` is nullable (read with `COALESCE(..., 80)`);
  `app.publish_event()` returns `void`; the `app.event_consumers` cursor table
  from §5.4/§9.3 is **absent** from the real `0003` SQL, so
  `internal/events.ensureConsumer` self-bootstraps it with
  `CREATE TABLE IF NOT EXISTS`.
- There is **no live database** in this repo and no integration-test setup —
  handlers compile/vet cleanly but cannot run. Write unit tests for pure
  functions only (e.g. `internal/budgets/progress_test.go`,
  `internal/fx/fetcher_test.go`).

## Conventions (all from ARCHITECTURE.md §8)

- Every mutating handler runs through `db.WithSerializable` (retries the whole
  closure on SQLSTATE `40001`/`40P01`); closures must contain only DB work.
  No email/push/HTTP inside — side effects go after COMMIT.
- Balance reversals (update/delete) sum `account.balance_ledger` by `ref_id`;
  never recompute from current FX rates. Lock multiple accounts in ascending
  id order. Transfers are two ledger legs, rejected by update.
- Money is integer minor units end-to-end (`common.Bigint` travels as a JSON
  string; Hasura runs with `STRINGIFY_NUMERIC_TYPES=true`); FX math via
  `common.ConvertToMinor` with each currency's `minor_unit`, never floats.
- Account types come from the `account.accounts_type` catalog (migration
  `000008`: `debit card`, `cash`, `deposit`, `broker`, `credit card`,
  `credit`, `debt`, `crypto`, `saving`); `account.accounts.type` is a FK to
  it. New accounts open with `balance_minor = initial_amount` plus an opening
  `balance_ledger` row when nonzero.
- New organizations are seeded by copying the global `tx.system_categories`
  template into `tx.categories` (migration `000009` removed `is_system` from
  `tx.categories`); see `internal/auth insertSystemCategories`.
- Use the shared helpers: `common.ActingOrg` for org scoping,
  `common.ValidCurrency(ctx, pool, code)` before inserts,
  `common.Wrap`/`BadRequest`/`NotFound` + `WriteError`/`WriteJSON` for errors.
  Handlers follow the `Handler{pool, log}` + `NewHandler(pool, cfg, log)` shape.
- Middleware (`internal/common`) only checks `X-Action-Secret` and recovers
  panics — `SET LOCAL app.via_action = 'on'` must be set inside each mutating
  handler's own transaction (a middleware connection cannot provide it).
  Empty secret disables verification (local dev).
- Event worker: `listener.go` uses a dedicated `pgx.Connect` (not the pool),
  advisory-lock leader election, LISTEN-first then catch-up. `dispatcher.go`
  processes one event per tx (idempotency via
  `analytics.processed_events`, cursor in `app.event_consumers`,
  trailing-window re-scan); domain writes join that tx and user notifications
  go through the `Notify` hook post-COMMIT (at-least-once).
- Weekly budget periods start Monday (matches the cron `0 0 * * 1`).
- Deploy: `Dockerfile` is a multi-stage static build; in
  `docker-compose.yml` the backend only `expose`s 9090 (never published —
  Action handlers trust Hasura-forwarded session vars). `.env` holds all
  secrets (§9.1 template in README) and is never committed; the Hasura/Go
  action secret is shared via `X-Action-Secret`.
- Useful skill: `golang-how-to` orchestrates the Go skill set for
  implementation questions.
