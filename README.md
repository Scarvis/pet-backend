# pet-backend

Go business-logic service for a Hasura + PostgreSQL finance app
(see `ARCHITECTURE.md`; `PLAN.md` tracks implementation progress).
It serves Hasura Actions/Events/Cron webhooks, recalculates balances,
budgets, holdings and analytics, and runs a `LISTEN`-based event worker.

## Prerequisites

- Go 1.23+ (module `github.com/Scarvis/pet-backend`)
- Docker + Docker Compose plugin (for the full stack)
- `golang-migrate` CLI (only for `make migrate-*`; not vendored here)
- A PostgreSQL 16 database and the real DDL from the sibling `db` repo
  (`db_schema_0001/0002/0003.sql`, applied in order). There is **no live
  database and no `migrations/` directory in this repo**, so DB-backed
  handlers compile and `go vet` cleanly but cannot run here.

## Configuration (`.env`, never committed)

Copy this template to `.env` (gitignored). Values are per
`ARCHITECTURE.md` §9.1; `docker-compose.yml` reads them via `${}`.

```env
# PostgreSQL
# (inside the compose network use sslmode=disable unless Postgres TLS is configured)
DB_PASSWORD=<password>
DATABASE_URL=postgres://app:<password>@postgres:5432/financeapp?sslmode=require

# Hasura
HASURA_GRAPHQL_ADMIN_SECRET=<secret>
HASURA_GRAPHQL_JWT_SECRET={"type":"RS256","key":"<public-pem>"}
HASURA_GRAPHQL_UNAUTHORIZED_ROLE=anonymous
HASURA_GRAPHQL_STRINGIFY_NUMERIC_TYPES=true   # bigint/numeric as strings (see ARCHITECTURE.md §6.7)
HASURA_ACTION_BASE_URL=http://backend:9090
HASURA_ACTION_SECRET=<secret>   # sent as X-Action-Secret; verified by the Go middleware

# Go service
LISTEN_ADDR=:9090
JWT_PRIVATE_KEY_PATH=/secrets/jwt_private.pem
JWT_ACCESS_TTL=15m
JWT_REFRESH_TTL=720h

# FX
FX_API_URL=https://api.exchangerate.host
FX_API_KEY=<key>

# Storage (import files)
STORAGE_BACKEND=local        # or "s3"
```

Notes:

- `HASURA_GRAPHQL_JWT_SECRET` carries the RS256 **public** key PEM; the Go
  service signs with the **private** key at `JWT_PRIVATE_KEY_PATH`.
- `HASURA_ACTION_SECRET` is shared: Hasura sends it as `X-Action-Secret`
  (`value_from_env` in Action/Event metadata) and the Go middleware
  (`internal/common/middleware.go`) verifies it.
- `LISTEN_ADDR` defaults to `:9090`; the compose `backend` service only
  exposes (not publishes) port 9090 — Action handlers must be reachable
  only over the compose network (§11).

## Run / migrate / test

PowerShell note: `&&` does not chain commands here — use `;`.

```powershell
go build ./...; go vet ./...; gofmt -l cmd internal graphql; go test ./...
```

- `go build ./...` — compile everything (must pass first).
- `go vet ./...` — static checks.
- `gofmt -l cmd internal graphql` — must print nothing (only
  `.agents/skills/` assets may be flagged; ignore those).
- `go test ./internal/<pkg>/` — single package, e.g.
  `go test ./internal/budgets/`.
- `go run ./cmd/server` (or `make run`) — start the service; needs
  `DATABASE_URL` + `JWT_PRIVATE_KEY_PATH` set.
- `make build` / `make test` (`go test -race -cover ./...`) — shortcuts.
- `make migrate-create name=<name>` / `make migrate-up` / `make migrate-down`
  — golang-migrate against `DATABASE_URL` (see `Makefile`).

Honest limitations of this checkout:

- `make lint` does not work here (no golangci config installed).
- `make migrate-*` does not work here (no `migrations/` dir yet, no live DB).
- `go test -race` needs a cgo toolchain; plain `go test ./...` works
  without one.

## HTTP routes (`cmd/server/main.go`, chi)

Public (no action secret):

- `GET /healthz` — `{"status":"ok"}` liveness probe.
- `GET /metrics` — Prometheus exposition (counters/timers per §10:
  `financeapp_http_requests_total`, `financeapp_events_*`,
  `financeapp_import_rows_total`, `financeapp_fx_fetch_*`,
  `financeapp_db_pool_in_use`).

Protected (shared-secret middleware, §6.3): every route below also sets
`SET LOCAL app.via_action = 'on'` and recovers panics into JSON errors.

- Actions (§6.4): `/actions/auth/*` (register, login, refresh),
  `/actions/account/create`, `/actions/tx/*` (create, transfer, update,
  delete), `/actions/invest/trade`, `/actions/import/start`,
  `/actions/fx/fetch`, `/actions/analytics/rebuild`.
- Events (§6.5): `/events/tx/*`, `/events/invest/trade`,
  `/events/import/job`, `/events/fx/rate`.
- Cron (§6.6): `/cron/fetch_fx_rates`, `/cron/rebuild_daily_summary`,
  `/cron/snapshot_net_worth`, `/cron/rotate_budget_progress`,
  `/cron/cleanup_tokens`.

## Hasura wiring

- Action/Event/Cron definitions live in `graphql/actions.graphql`
  (inputs, `Bigint` scalar, endpoints table). Point Hasura Actions at
  `HASURA_ACTION_BASE_URL` (`http://backend:9090` in compose) with the
  `X-Action-Secret` header from `HASURA_ACTION_SECRET`.
- Amounts cross the wire as the string-backed `Bigint` scalar; Hasura runs
  with `HASURA_GRAPHQL_STRINGIFY_NUMERIC_TYPES=true` so `bigint`/`numeric`
  columns return as strings too (§6.7).
- Tracked objects, roles (`owner`/`admin`/`member`/`viewer`) and the
  `app.via_action` guard trigger are described in §6.1–§6.3.

## Docker Compose

```powershell
docker compose up --build
```

- `postgres:16-alpine` — data in the `pgdata` volume; schema seeded from
  `./db_schema.sql` on first init; health-checked via `pg_isready`.
  No published port (add `127.0.0.1:5432:5432` locally for debugging).
- `hasura/graphql-engine:v2.45.0` — on `http://localhost:8080`
  (admin secret from `.env`); waits for Postgres `service_healthy`.
- `backend` — built from `Dockerfile` (multi-stage: `golang:1.23-alpine`
  static build with `CGO_ENABLED=0`, minimal alpine runtime,
  `EXPOSE 9090`, `ENTRYPOINT /app/server`); starts after Postgres is
  healthy; port 9090 exposed to the compose network only.

CI/CD shape (§9.4): `golangci-lint` → `go test -race -cover` →
`docker build` → migrations on staging → integration tests
(testcontainers + Hasura) → push → rollout.
