# pet-backend

Go business-logic service for a Hasura + PostgreSQL finance app
(see `ARCHITECTURE.md`; `PLAN.md` tracks implementation progress).
It serves Hasura Actions/Events/Cron webhooks, recalculates balances,
budgets, holdings and analytics, and runs a `LISTEN`-based event worker.

## Prerequisites

- Go 1.26+ (`go.mod` directive `go 1.26.0`)
- Docker Engine + Compose plugin (the full stack is compose-tested:
  Hasura v2.45.0 + PostgreSQL 16 + backend)
- `golang-migrate` CLI (only for `make migrate-*`; not vendored here)
- The real DDL from the sibling `db` repo
  (`db_schema_0001..0004.sql`, applied in order; `0004` adds the
  `app.via_action` guard trigger on `tx.transactions`). There is no
  `migrations/` directory in this repo — `db_schema.sql` is the merged
  baseline the compose Postgres seeds from on first init.

## Configuration (`.env`, never committed)

Copy this template to `.env` (gitignored). Values are per
`ARCHITECTURE.md` §9.1; `docker-compose.yml` reads them via `${}`.

```env
# PostgreSQL
# (compose network has no TLS: use sslmode=disable here; sslmode=require
# only against a TLS-configured / managed Postgres)
DB_PASSWORD=<password>
DATABASE_URL=postgres://app:<password>@postgres:5432/financeapp?sslmode=disable

# Hasura
HASURA_GRAPHQL_ADMIN_SECRET=<secret>
HASURA_GRAPHQL_JWT_SECRET={"type":"RS256","key":"<public-pem>"}
HASURA_GRAPHQL_UNAUTHORIZED_ROLE=anonymous
HASURA_GRAPHQL_STRINGIFY_NUMERIC_TYPES=true   # bigint/numeric as strings (see ARCHITECTURE.md §6.7)
HASURA_ACTION_BASE_URL=http://backend:9090
HASURA_ACTION_SECRET=<secret>   # sent as X-Action-Secret; verified by the Go middleware

# Go service
LISTEN_ADDR=:9090
JWT_PRIVATE_KEY_PATH=/app/jwt_private.pem   # compose mounts ./jwt_private.pem here (read-only)
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
- Two env profiles, never mixed in one `.env`: `.env.compose.example`
  (compose: `postgres` host, `sslmode=disable`) vs a localhost profile for
  bare-backend dev (`localhost`, `sslmode=disable`). The backend requires
  `JWT_PRIVATE_KEY_PATH` at startup (config hard-fails without it); the key
  itself is lazy-loaded on first auth request.

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

- `make lint` does not work here (`make` itself is not installed in this
  WSL distro, and there is no golangci config).
- `make migrate-*` does not work here (no `migrations/` dir — DDL sources
  live in the sibling `db` repo; no live DB reachable from the host since
  compose publishes no Postgres port).
- `go test -race` needs a cgo toolchain; plain `go test ./...` works
  without one.

## HTTP routes (`cmd/server/routes.go`, chi)

Public (no action secret):

- `GET /healthz` — `{"status":"ok"}` liveness probe.
- `GET /metrics` — Prometheus exposition (counters/timers per §10:
  `financeapp_http_requests_total`, `financeapp_events_*`,
  `financeapp_import_rows_total`, `financeapp_fx_fetch_*`,
  `financeapp_db_pool_in_use`).

Protected (shared-secret middleware, §6.3): the middleware checks
`X-Action-Secret` (401 on missing/invalid) and recovers panics into JSON
errors. Mutating tx handlers additionally set
`SET LOCAL app.via_action = 'on'` inside their own transaction so the
`trg_tx_via_action` guard lets the write through.

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
- The applied §6 configuration is committed as code under
  `hasura/metadata/` (tracked tables/views, `owner`/`admin`/`member`/`viewer`
  permissions incl. Action permissions, 12 Actions, 5 event triggers,
  5 cron triggers). Edit the SDL first, then re-apply — never hand-edit in
  console and drift. Cron payloads are `{}`: the backend unwraps Hasura's
  `{name, scheduled_time, payload}` envelope (`decodeInput` in
  `cmd/server/main.go`).
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
- `backend` — built from `Dockerfile` (multi-stage: `golang:1.26-alpine`
  static build with `CGO_ENABLED=0`, minimal alpine runtime,
  `EXPOSE 9090`, `ENTRYPOINT /app/server`); starts after Postgres is
  healthy and Hasura has started; port 9090 exposed to the compose network
  only. Requires `JWT_PRIVATE_KEY_PATH` (compose mounts the gitignored
  `./jwt_private.pem` at `/app/jwt_private.pem`, read-only).

CI/CD shape (§9.4): `golangci-lint` → `go test -race -cover` →
`docker build` → migrations on staging → integration tests
(testcontainers + Hasura) → push → rollout.
