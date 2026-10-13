# Hasura + GraphQL — Configure & Run Plan

Goal: bring up Hasura v2.45.0 against PostgreSQL 16 and wire it to the Go
backend (`cmd/server`, `graphql/actions.graphql`) per `ARCHITECTURE.md` §6, §9.

## 0. Current state (verified 2026-10-13)

- `docker-compose.yml` defines `postgres` + `hasura` + `backend`. DB seed is
  resolved: `./db_schema.sql` (the path compose mounts) is a merged baseline
  generated from `migrations/db_schema_0001..0003.sql`, in order — do not
  edit it by hand, edit `migrations/` and re-merge. Source of truth for the
  DDL remains the SQL files (trust SQL over prose on conflicts, see
  `AGENTS.md`). Fresh volume required for init scripts to run
  (`docker compose down -v` to re-init).
- `.env` is local-dev only: `DATABASE_URL` points at
    `localhost:5432/postgres`, Hasura vars are commented out. Compose needs
    `DATABASE_URL=postgres://app:<pw>@postgres:5432/financeapp?sslmode=disable`
    (no TLS inside the compose network; `sslmode=require` in committed
    templates is wrong for stock `postgres:16-alpine`).
- `graphql/actions.graphql` holds Action types/inputs (`Bigint` scalar, 13
  mutations) but there is **no Hasura metadata/** directory and no CLI config
  — all Hasura config currently lives only in prose (§6.2–§6.6).
- Backend routes are fixed (`cmd/server/routes.go`): Actions `/actions/*`,
  Events `/events/*`, Cron `/cron/*` on `:9090`, exposed (not published) in
  compose; handlers verify `X-Action-Secret` and set
  `SET LOCAL app.via_action='on'` per-tx.

## 1. Prerequisites (Ubuntu 26.04, bash)

- Docker Engine + Compose plugin, Go 1.23+, Hasura CLI v2. Install:
  ```bash
  # Docker (official repo; $(lsb_release -cs) resolves the 26.04 suite —
  # if Docker has no suite for it yet, see Docker docs and pin nearest LTS)
  sudo apt update && sudo apt install -y ca-certificates curl gnupg
  sudo install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
    https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" \
    | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
  sudo apt update && sudo apt install -y docker-ce docker-ce-cli \
    containerd.io docker-buildx-plugin docker-compose-plugin
  sudo usermod -aG docker "$USER"   # re-login afterwards

  # Go (snap tracks latest stable; needs 1.23+ for the go 1.26.0 directive)
  sudo snap install go --classic && go version

  # Hasura CLI v2
  curl -L https://github.com/hasura/graphql-engine/raw/stable/cli/get.sh | bash
  hasura --version
  ```
- JWT keypair: private PEM for Go (`JWT_PRIVATE_KEY_PATH`), public PEM for
  Hasura `HASURA_GRAPHQL_JWT_SECRET`. Existing `jwt_private.pem` in repo root
  is dev-only — regenerate for anything shared:
  ```bash
  openssl genrsa -out jwt_private.pem 2048
  openssl rsa -in jwt_private.pem -pubout -out jwt_public.pem
  # single-line value for HASURA_GRAPHQL_JWT_SECRET {"type":"RS256","key":"..."}:
  awk 'NF {sub(/\r$/,""); printf "%s\\n", $0}' jwt_public.pem
  ```
- Ports free: `8080` (Hasura console/API), `9090` reachable only inside the
  compose network. Check: `ss -ltn | grep -E ':(8080|9090|5432)' || true`.

## 2. Secrets / `.env` (never committed)

Fill the commented Hasura block in `.env` (values per §9.1):

```env
DB_PASSWORD=<strong-pw>
DATABASE_URL=postgres://app:<strong-pw>@postgres:5432/financeapp?sslmode=disable
HASURA_GRAPHQL_ADMIN_SECRET=<admin-secret>
HASURA_GRAPHQL_JWT_SECRET={"type":"RS256","key":"<public-pem-single-line>"}
HASURA_ACTION_SECRET=<action-secret>   # = backend HASURA_ACTION_SECRET
HASURA_ACTION_BASE_URL=http://backend:9090
```

Notes:

- JWT claims shape is fixed (§8.1): `x-hasura-allowed-roles`,
  `x-hasura-default-role`, `x-hasura-org-id`, `x-hasura-user-id` under
  `https://hasura.io/jwt/claims`. `HASURA_GRAPHQL_UNAUTHORIZED_ROLE=anonymous`,
  `HASURA_GRAPHQL_STRINGIFY_NUMERIC_TYPES=true` (else `bigint` amounts lose
  precision; wire type is string-backed `Bigint`, §6.7).
- Backend `HASURA_ACTION_SECRET=dev-secret` in current `.env` must match
  Hasura's `value_from_env: HASURA_ACTION_SECRET` header on every
  Action/Event/Cron webhook. Empty secret disables verification (local dev
  only).
- Keep two env profiles: local bare-backend (`localhost`, `sslmode=disable`)
  vs compose (`postgres` host). Do not mix them in one `.env`.

## 3. Database bring-up (before Hasura)

1. Seed is in place (`./db_schema.sql`, merged from `migrations/`). If
   `migrations/` changes, re-merge the three files in order and recreate the
   Postgres volume — init scripts run only on first init
   (`docker compose down -v` to re-init).
2. `docker compose up postgres` and wait for `service_healthy`
   (`pg_isready -U app -d financeapp`). For local debugging add
   `127.0.0.1:5432:5432` port mapping (do not commit an open port).
3. Verify: tables across `app/identity/fx/account/tx/budget/invest/import/
   analytics`, `app.events.uid`, `app.event_consumers` (or confirm Go
   self-bootstraps it), `app.via_action` guard trigger on `tx.transactions`.

## 4. Run

```bash
docker compose up --build
# Hasura console: http://localhost:8080 (use HASURA_GRAPHQL_ADMIN_SECRET)
# Backend :9090 reachable only inside compose network (by design, §11)
```

`hasura` waits for `postgres healthy`; `backend` builds from `Dockerfile`
(static `CGO_ENABLED=0` build). Check `GET /healthz` from inside the network
(`docker compose exec backend wget -qO- http://localhost:9090/healthz`).

Hasura CLI workflow (do once, then commit the output):

```bash
hasura init hasura --endpoint http://localhost:8080 --admin-secret <secret>
cd hasura && hasura console   # all clicks below land in hasura/metadata/
```

## 5. Configure Hasura (console → metadata as code)

Do in this order; export metadata after each block.

1. **Data source:** connect `HASURA_GRAPHQL_DATABASE_URL` (default source).
2. **Track (§6.2):** track all tables/views except `identity.refresh_tokens`
   (Go-only). Views `account.v_active_accounts`, `tx.v_transactions_full`
   as read-only; `account.balance_ledger`, `budget.budget_progress`,
   `invest.holdings`, `import.raw_rows`, `analytics.*`, `fx.rates` read-only
   for non-admin roles (written only from Go/cron). Hide
   `identity.users.password_hash`. Add FK relationships
   (account→ledger, tx→category/tags, budget→limits→progress,
   portfolio→holdings→trades, job→raw_rows).
3. **Permissions (§6.3):** roles `owner/admin/member/viewer` (from
   `identity.roles`). Row filter `{org_id: {_eq: "X-Hasura-Org-Id"}}` on every
   tenant table. `tx.transactions`: select for all roles, update limited to
   `(category_id, description)` for member, **no delete** and `deleted_at` not
   updatable — deletes go through `deleteTransaction`. Rely on the
   `app.via_action` trigger as defense-in-depth, not as the permission model.
4. **Custom types:** add `scalar Bigint` + all inputs/enums/outputs from
   `graphql/actions.graphql` (§6.7) into Hasura custom types.
5. **Actions (§6.4, 13 total):** one per `type Mutation` field; handler URL =
   `HASURA_ACTION_BASE_URL` + path in `routes.go`
   (`/actions/auth/register|login|refresh`, `/actions/account/create`,
   `/actions/tx/create|transfer|update|delete`, `/actions/invest/trade`,
   `/actions/import/start`, `/actions/fx/fetch`, `/actions/analytics/rebuild`).
   Forward `X-Action-Secret` from env, forward session vars
   (`X-Hasura-User-Id`, `X-Hasura-Org-Id`). Derive Action from
   `graphql/actions.graphql` (do not hand-edit SDL in console and drift).
6. **Events (§6.5):** 5 webhooks → backend `/events/*`
   (`transaction_inserted/updated`, `trade_inserted`, `import_job_updated`,
   `fx_rate_inserted`). Headers: `X-Action-Secret`. Retry + dead-letter config;
   note Go treats these as catch-up triggers draining `app.events` (§8.8),
   so at-least-once is expected.
7. **Cron (§6.6):** 5 triggers with payloads matching `routes.go` cron inputs:
   `fetch_fx_rates (0 6 * * *)`, `rebuild_daily_summary (30 0 * * *)`,
   `snapshot_net_worth (0 1 * * *)`, `rotate_budget_progress (0 0 * * 1,
   Monday)`, `cleanup_tokens (0 3 * * 0)`. Same secret header.
8. Commit `hasura/metadata/` + `hasura/migrations/` to git; future
   environments run `hasura metadata apply && hasura migrate apply`.

## 6. Verify

- Console GraphiQL as a JWT user: `ActiveAccounts`, `Transactions`,
  `Balance` subscription examples (§6.7); confirm `balance_minor` arrives as
  string.
- Actions end-to-end: `register` → use access token → `createAccount` →
  `createTransaction` → check `balance_minor` + `balance_ledger` row. Direct
  `UPDATE tx.transactions SET amount_minor` via GraphQL must fail (guard
  trigger without `app.via_action`).
- Negative: wrong/missing `X-Action-Secret` → 401/403 from backend
  (`internal/common/middleware.go`).
- Events: insert tx, confirm `app.events` row + worker catch-up
  (`analytics.processed_events`, `financeapp_events_*` metrics on
  `GET /metrics`).
- Cron: manually trigger each once from console; check `fx.rates`,
  `daily_category_summary`, `net_worth_snapshots`, token cleanup.

## 7. Open items / decisions

- `sslmode=require` in README/ARCHITECTURE templates breaks stock compose
  Postgres (no TLS) — use `disable` inside the network, `require` only with
  TLS configured.
- `BIGINT` range note (§1, App. A): unchanged by this plan; amounts stay
  string-`Bigint` on the wire regardless.
- `make lint` does not work in this checkout (no golangci config) —
  Hasura migrations live under `hasura/migrations/` once §4 CLI step is done.
  DB migrations live in `migrations/` (`make migrate-*` needs a live
  `DATABASE_URL`).

## Definition of done

`docker compose up --build` green → console on `:8080` → all §6.2 objects
tracked with §6.3 permissions → 13 Actions + 5 Events + 5 Crons green against
backend `:9090` → metadata committed → §6.7 smoke queries pass.
