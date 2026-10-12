# Go Backend — Implementation Plan

Scaffolded from `ARCHITECTURE.md` §7.1 (all directories + empty stubs exist).
This plan fills in the remaining work, ordered so each phase builds on the previous one.
All business-logic details below are taken verbatim from `ARCHITECTURE.md`.

---

## Phase 0 — Module & tooling foundation

- [x] Create `go.mod` (module path e.g. `github.com/financeapp/backend`, Go version). ✅ module `github.com/Scarvis/pet-backend`, go 1.23.0.
- [x] Add dependencies: `github.com/jackc/pgx/v5` (+ `pgxpool`), `github.com/go-chi/chi/v5`, `log/slog`. ✅ both in `go.mod` + `go.sum`; stdlib `slog` present. Later deps (bcrypt, JWT, Prometheus) deferred per §2.
- [x] `.gitignore`: add `/bin`, `/dist`, `.env`, `*.pem`, IDE files. ✅ all entries present.
- [x] Verify the stubs parse: `go build ./...` should compile empty packages; run `gofmt -l .`. ✅ `go build ./...` exit 0, `go vet ./...` clean, our code gofmt-clean (only `.agents/skills/…` skill assets are flagged).

## Phase 1 — Config + DB plumbing (no domain logic yet)

- [x] `internal/config/`: load env vars + flags, validate, expose typed config (`LISTEN_ADDR`, `DATABASE_URL`, `JWT_*`, `FX_API_*`, `STORAGE_BACKEND`). §9.1 — created `config.go`; validates required fields + storage-backend enum.
- [x] `internal/db/pool.go`: pgx `pool.NewWithConfig` from `DATABASE_URL`; connection health check on startup. §7.1 — sets max/min conns, `MaxConnLifetime`, pings before returning.
- [x] `internal/db/tx.go`: implement `WithSerializable(ctx, pool, fn)` — retry whole closure on SQLSTATE `40001` / `40P01`, jittered exponential backoff (5 ms base). §8.0 — uses `math/rand/v2`.
- [x] `Makefile` targets: `migrate-create/up/down`, `build`, `test`, `lint`, `run`. §9.3 — POSIX Makefile (uses golang-migrate); recipes tab-indented; `DATABASE_URL` read from env or `.env`.

## Phase 2 — Common helpers (shared by all domains)

- [x] `internal/common/errors.go`: typed errors + retryable sentinel (`ErrRetryable`) → HTTP 409 after retries. §8.0 — added `Error` struct (Status/Code/Message) with constructors, `HTTPStatus(err)` mapper, and JSON error writer used by router + middleware.
- [x] `internal/common/money.go`: minor-unit conversion respecting each currency's `minor_unit` (JPY 0, BTC 8, ETH 18); string-backed `Bigint`. §7.1, §6.7 — `Bigint` marshals/unmarshals as a quoted decimal string; `ToMinor`/`FromMinor` use `big.Float` for precision on high-decimal currencies; `ConvertToMinor` handles cross-currency (§8.2 step 3).
- [x] `internal/common/middleware.go`: verify Hasura `X-Action-Secret` header; set `SET LOCAL app.via_action = 'on'`; recover panics → HTTP error. §6.3, §9.1 — wraps any http.Handler (works with chi.Router); ConnProvider interface so db.Pool wires in directly.

## Phase 3 — Auth (identity)

- [x] `internal/auth/password.go`: bcrypt hash/compare (cost ≥ 12). §8.1 — `Hash` uses cost 12; `Compare` is constant-time via `bcrypt.CompareHashAndPassword`. stdlib-only crypto (`golang.org/x/crypto/bcrypt`).
- [x] `internal/auth/jwt.go`: RS256 issue/verify; claims with `x-hasura-*` + `exp`. §8.1 — pure stdlib (no external JWT lib): PKCS#1v15 sign/verify over SHA-256 of base64url(header).payload; loads RSA key from PEM (PKCS#1 or PKCS#8); claims carry `sub`, the Hasura claim block (`x-hasura-org-id`/`user-id`/`default-role`/`allowed-roles`) and iat/exp.
- [x] `internal/auth/handler.go`: `register`, `login`, `refreshToken` handlers (transaction flows in §8.1). §7.1, §6.4 — `Register` runs one SERIALIZABLE tx (`WithSerializable`) creating org + user + owner role + system categories, then issues JWT + stores refresh token; `Login` verifies password and rotates a new pair; `RefreshToken` revokes the old token inside a tx and issues fresh tokens after commit (side effects post-COMMIT per §8.0). Lazy-cached RSA signing key.
- [x] Tests: `internal/auth/jwt_test.go` — bcrypt round-trip, JWT issue/verify round-trip + tamper/wrong-key rejection, PKCS#8 PEM load. All 4 pass; `go build ./...`, `go vet ./...`, gofmt clean on cmd/internal/graphql.

> **Note:** handlers reference the documented schema (identity.organizations/users/roles/user_roles, tx.categories, identity.refresh_tokens). They compile and vet cleanly now but will only run once `db_schema.sql` / migration 0001 are filled in — an open item (see Phase 9). JWT uses stdlib RS256 rather than a third-party library.

## Phase 4 — Transactions & accounts (core)

- [ ] `internal/accounts/`: account CRUD + archiving (`deleted_at`). §7.1
- [ ] `internal/transactions/handler.go`: create / update / delete transaction handlers. §8.2
- [ ] `internal/transactions/balance.go`: balance recalculation, FX conversion via nearest rate. §8.2
- [ ] `internal/transactions/transfer.go`: two-sided transfer with currency conversion + ledger rows. §8.3
- [ ] Wire SERIALIZABLE retry (§Phase 1) around every mutating handler; lock accounts in ascending id order to avoid deadlocks. §8.0, §8.2

## Phase 5 — Budgets & analytics (event-driven recalculation)

- [ ] `internal/budgets/handler.go`: budget CRUD handlers. §7.1
- [ ] `internal/budgets/progress.go`: `budget_progress` upsert on transaction events; recursive category tree; alert publishing. §8.4
- [ ] `internal/analytics/daily_summary.go`: rebuild `daily_category_summary` (exclude transfers, delete-then-insert). §8.7
- [ ] `internal/analytics/net_worth.go`: snapshot net worth in base currency at latest rates. §8.7

## Phase 6 — Invest & imports (async workers)

- [ ] `internal/invest/handler.go`, `trade.go`, `valuation.go`: trade execution, holdings recalc, position valuation via `price_history`. §8.5
- [ ] `internal/imports/parser_csv.go`, `parser_ofx.go`: row parsing; dedup hash with FITID / occurrence index. §8.6
- [ ] `internal/imports/dedup.go`, `mapper.go`: cross-job duplicate lookup, map raw rows → transactions. §8.6

## Phase 7 — FX fetcher + event worker (LISTEN)

- [ ] `internal/fx/handler.go`, `fetcher.go`: daily rate load with `ON CONFLICT DO UPDATE`; trigger publishes event. §8.7
- [ ] `internal/events/listener.go`: reconnect loop, advisory-lock leader election (`pg_try_advisory_lock`), LISTEN-first + catch-up. §8.8
- [ ] `internal/events/dispatcher.go`: per-event transaction (idempotency via `analytics.processed_events`, cursor update). §8.8

## Phase 8 — Wiring: router, entrypoint, Hasura definitions

- [ ] `cmd/server/main.go`: build chi router for all Actions/Events/Cron endpoints; start LISTEN worker goroutine. §7.1
- [ ] `graphql/actions.graphql`: Action + Event + Cron + custom scalar (`Bigint`) definitions matching §6.4–6.7.
- [ ] Observability: Prometheus counters/timers per §10 metric names (http requests, events processed/failed/lag, import rows, fx fetch).

## Phase 9 — Deployment & docs

- [ ] `Dockerfile`: multi-stage Go build → static binary. §9.2
- [ ] `docker-compose.yml`: postgres + hasura + backend services, health checks, secrets via env, action secret shared between Hasura/Go. §9.2
- [ ] Update README with run/migrate/test instructions and the `.env` template from §9.1.

---

## Dependencies / ordering notes

- Phases 3–7 all depend on Phase 1 (DB) and Phase 2 (common helpers).
- The event worker (Phase 7) is what ties recalculation domains together; implement it after the domain handlers exist so `Dispatcher` has real work to call.
- Keep each mutating handler inside a single DB transaction with side effects (email/push) deferred until **after** COMMIT, per §8.0 / §8.8 idempotency rules.
