# FinanceApp — Backend Architecture

> **Stack:** Go · GraphQL · Hasura · PostgreSQL (monolith)
> **Source:** `db_schema.sql` — a single instance, 9 schemas, LISTEN/NOTIFY instead of Kafka
> **Document version:** 2026-10-12 (rev. 2 — see Appendix A)

---

## 1. Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                          CLIENT LAYER                               │
│     Web (SPA)  ·  Mobile (iOS/Android)  ·  CLI / integrations       │
└───────────────────────────┬─────────────────────────────────────────┘
                            │  HTTPS / WSS
                            ▼
┌─────────────────────────────────────────────────────────────────────┐
│                      HASURA GRAPHQL ENGINE                          │
│                                                                     │
│  • GraphQL API (queries / mutations / subscriptions)                │
│  • JWT authentication, roles: owner / admin / member / viewer       │
│  • Permissions with filtering by org_id                             │
│  • Actions  → synchronous calls to Go business-logic handlers       │
│  • Events   → asynchronous webhooks to Go workers                   │
│  • Cron     → scheduled tasks (rates, analytics, snapshots)         │
└──────────┬──────────────────────────────────────────────────────────┘
           │  SQL (pgx) / LISTEN app_events
           ▼
┌─────────────────────────────────────────────────────────────────────┐
│               POSTGRESQL — single source of truth                   │
│                                                                     │
│  app · identity · fx · account · tx · budget · invest · import      │
│  analytics                                                          │
│                                                                     │
│  • app.events      — event log (source of truth)                    │
│  • pg_notify(...)  — `app_events` channel for instant delivery      │
│  • Triggers        — auto-publishing of events + updated_at         │
└──────────▲──────────────────────────────────────────────────────────┘
           │
┌──────────┴──────────────────────────────────────────────────────────┐
│                    GO BUSINESS-LOGIC SERVICE                        │
│                                                                     │
│  auth · accounts · transactions · budgets · invest · imports        │
│  fx · analytics · event-worker (LISTEN app_events)                  │
└─────────────────────────────────────────────────────────────────────┘
```

### Key Principles

| # | Principle | Implementation |
|---|-----------|----------------|
| 1 | Single source of truth | One PostgreSQL, real `FOREIGN KEY`s across schemas |
| 2 | Event-driven without Kafka | `app.events` + `pg_notify('app_events', …)` |
| 3 | Business logic in Go | Balances, budgets, holdings, import — in Go only |
| 4 | GraphQL-first | Hasura generates the API from tables and views |
| 5 | Soft delete | `deleted_at` + triggers in `'soft'` mode |
| 6 | Money in `BIGINT` | All amounts in minor units (`*_minor`), respecting the currency's `minor_unit` (JPY — 0, BTC — 8, ETH — 18) |
| 7 | Idempotency | `analytics.processed_events` stores processed events |

> **Note on the `BIGINT` range.** `BIGINT` holds up to ≈ 9.22·10¹⁸, which is
> only ≈ 9.22 ETH at 18 decimals. If assets with `minor_unit` > 8 really are
> going to be stored, keep their `*_minor` columns as `NUMERIC(38,0)` (and use
> `math/big` in Go) or cap the stored precision — this is an open decision
> (see Appendix A). Over the API, all `*_minor` values travel as the
> string-backed `Bigint` scalar (see 6.7): GraphQL `Int` is 32-bit and
> JavaScript numbers are exact only up to 2⁵³.

---

## 2. Technology Stack

| Layer | Technology | Purpose |
|-------|-----------|---------|
| API gateway | **Hasura GraphQL Engine v2.x** | CRUD, subscriptions, permissions, delegation |
| Business logic | **Go** (pgx, chi, slog) | Actions, Events, Cron, LISTEN worker |
| Database | **PostgreSQL 16** (pgcrypto, citext) | Data, triggers, LISTEN/NOTIFY |
| Authentication | JWT (RS256), access + refresh | `identity.refresh_tokens` |
| Password hashing | **bcrypt** (cost ≥ 12) | `identity.users.password_hash` |
| Email | `CITEXT` | Case-insensitive unique email |
| UUID | `pgcrypto → gen_random_uuid()` | All primary keys of business tables |
| Migrations | `golang-migrate` / `goose` | Schema versioning |
| Observability | Prometheus + Grafana, Loki, OpenTelemetry | Metrics, logs, traces |

---

## 3. Database Schema

### 3.1 Schema Overview

| Schema | Purpose | Main objects |
|--------|---------|--------------|
| `app` | Service schema: events, utilities | `events`, `publish_event()`, `event_notify()`, `set_updated_at()` |
| `identity` | Users, organizations, roles | `organizations`, `users`, `roles`, `user_roles`, `refresh_tokens` |
| `fx` | Currencies and exchange rates | `currencies`, `rates` |
| `account` | Accounts and balances | `accounts`, `balance_ledger`, `v_active_accounts` (view) |
| `tx` | Transactions, categories, tags | `categories`, `transactions`, `tags`, `transaction_tags`, `v_transactions_full` (view) |
| `budget` | Budgets and limits | `budgets`, `budget_limits`, `budget_progress` |
| `invest` | Investments | `portfolios`, `assets`, `holdings`, `trades`, `price_history` |
| `import` | Statement import | `import_jobs`, `raw_rows` |
| `analytics` | Read models | `daily_category_summary`, `net_worth_snapshots`, `processed_events` |

### 3.2 Key Constraints (accounted for in business logic)

| Table | Constraint | Impact on code |
|-------|-----------|----------------|
| `tx.transactions` | `amount_minor > 0` | Go validates the amount before insert |
| `tx.transactions` | `kind='transfer' → to_account_id IS NOT NULL` | A transfer requires a second leg |
| `tx.transactions` | `kind='transfer' → category_id IS NULL` | A transfer has no category |
| `tx.transactions` | `kind='transfer' → account_id <> to_account_id` | Transfers to the same account are forbidden |
| `tx.categories` | `UNIQUE (org_id, parent_id, name)` | Duplicate category control |
| `budget.budget_limits` | `UNIQUE (budget_id, category_id)` | One limit per category within a budget |
| `invest.holdings` | `UNIQUE (portfolio_id, asset_id)` | One position per asset in a portfolio |
| `invest.trades` | FK → `holdings(id)` | A trade is always tied to a position |
| `import.raw_rows` | `UNIQUE (import_job_id, dedup_hash)` | Deduplication within one job; the hash includes an occurrence index (see 8.6); duplicates across jobs are found by lookup |
| `fx.rates` | `UNIQUE (base_code, quote_code, as_of)` | One rate per pair per date |
| `identity.users` | `email CITEXT UNIQUE` | Case-insensitive uniqueness |

### 3.3 Pseudo-enum Fields (CHECK constraints)

| Table | Field | Allowed values |
|-------|-------|----------------|
| `identity.organizations` | `plan` | `free`, `pro`, `team` |
| `identity.users` | `status` | `active`, `suspended`, `deleted` |
| `account.accounts` | `type` | `cash`, `card`, `deposit`, `debt` |
| `tx.categories` | `kind` | `income`, `expense` |
| `tx.transactions` | `kind` | `income`, `expense`, `transfer` |
| `tx.transactions` | `source` | `manual`, `import`, `recurring` |
| `account.balance_ledger` | `reason` | `transaction`, `adjustment`, `transfer` |
| `budget.budgets` | `period` | `monthly`, `weekly`, `custom` |
| `invest.assets` | `asset_type` | `stock`, `crypto`, `deposit`, `bond` |
| `invest.trades` | `side` | `buy`, `sell` |
| `import.import_jobs` | `file_format` | `csv`, `ofx` |
| `import.import_jobs` | `status` | `pending`, `parsing`, `mapping`, `completed`, `failed` |
| `import.raw_rows` | `status` | `pending`, `mapped`, `duplicate`, `error` |

---

## 4. Data Model

```
identity.organizations  1───N  identity.users
identity.users          N───N  identity.roles            (user_roles)
identity.users          1───N  identity.refresh_tokens

fx.currencies           1───N  fx.rates (base_code)
fx.currencies           1───N  fx.rates (quote_code)

identity.organizations  1───N  account.accounts
identity.users          1───N  account.accounts (owner_user_id)
account.accounts        1───N  account.balance_ledger
account.balance_ledger  N───1  tx.transactions (ref_id, ON DELETE SET NULL)

account.accounts        1───N  tx.transactions (account_id)
account.accounts        1───N  tx.transactions (to_account_id)
tx.categories           1───N  tx.transactions
tx.categories           1───N  tx.categories   (parent_id — tree)
tx.transactions         N───N  tx.tags         (transaction_tags)
import.import_jobs      1───N  tx.transactions (import_job_id, SET NULL)

budget.budgets          1───N  budget.budget_limits
tx.categories           1───N  budget.budget_limits
budget.budget_limits    1───1  budget.budget_progress (PK = budget_limit_id)

invest.portfolios       1───N  invest.holdings
invest.assets           1───N  invest.holdings
invest.holdings         1───N  invest.trades
invest.assets           1───N  invest.price_history

import.import_jobs      1───N  import.raw_rows
import.raw_rows         N───1  tx.transactions (mapped_transaction_id)

identity.organizations  1───N  analytics.daily_category_summary
identity.organizations  1───N  analytics.net_worth_snapshots

app.events               — log of all events (all aggregates)
```

---

## 5. Event-Driven Architecture

### 5.1 Mechanism (already implemented in the schema)

- `app.publish_event(aggregate_type, aggregate_id, event_type, payload)` —
  writes a row to `app.events` and calls `pg_notify('app_events', …)`.
- `app.event_notify(aggregate, mode)` — a universal trigger:
  - `mode = 'soft'` → when `deleted_at` is set, emits `….deleted`;
  - `mode = 'hard'` → only physical INSERT/UPDATE/DELETE.
- Truncation of large payloads: if `octet_length > 7000` bytes,
  the notification keeps only `event_type` + `aggregate_id` and a
  `truncated: true` flag, and the worker reads the remaining data itself.

### 5.2 Event Flow

```
 Business operation (Go handler or Hasura mutation)
        │
        ▼
 INSERT/UPDATE/DELETE on a business table
        │
        ▼  trigger → app.event_notify()
        │
        ├──► INSERT INTO app.events          (log, source of truth)
        │
        └──► pg_notify('app_events', payload)
                   │
                   ▼
             Go Event Worker (LISTEN app_events)
                   │
                   ├──► Recalculate budget.budget_progress
                   ├──► Update analytics.*
                   ├──► Notifications (budget alerts)
                   └──► Write to analytics.processed_events (idempotency)
```

### 5.3 Event Catalog (based on the schema's triggers)

| Aggregate | Mode | Events | Handling in Go |
|-----------|------|--------|----------------|
| `user` | soft | created / updated / deleted | audit |
| `account` | soft | created / updated / deleted | `net_worth` snapshots |
| `balance_ledger` | hard | created / deleted | audit, debugging |
| `category` | hard | created / updated / deleted | recalculation of budget trees |
| `transaction` | soft | created / updated / deleted | recalculation of budget and analytics |
| `tag` | hard | created / updated / deleted | — |
| `budget` | hard | created / updated / deleted | reset of `budget_progress` |
| `budget_limit` | hard | created / updated / deleted | initialization of `budget_progress` |
| `portfolio` | hard | created / updated / deleted | — |
| `holding` | hard | created / updated / deleted | recalculation of portfolio value |
| `trade` | hard | created / updated / deleted | recalculation of `holdings` |
| `price_history` | hard | created / deleted | revaluation of positions |
| `import_job` | hard | created / updated / deleted | start mapping on status change |
| `raw_row` | hard | created / updated / deleted | import monitoring |
| `fx_rate` | hard | created / deleted | recalculation of multi-currency aggregates |

### 5.4 Guarantees

| Aspect | Solution |
|--------|----------|
| Delivery | `app.events` is a WAL-protected table and the source of truth; `NOTIFY` is only a wake-up signal. The worker stores a cursor (`app.event_consumers.last_event_id`) and, after any crash or reconnect, re-reads everything after it |
| Idempotency | `analytics.processed_events` stores the `uid`s of processed events; the check, the handler's writes and the cursor update happen in one DB transaction |
| Ordering | `app.events.id BIGSERIAL`; events are processed in ascending `id`. Sequence values can become visible out of order under concurrent transactions, so every catch-up re-scans a trailing window (e.g. the last 1000 ids) and `processed_events` filters out what was already handled |
| Large data | The trigger truncates a notification `payload` > 7000 bytes (`truncated: true`); this is harmless, because the worker reads full rows from `app.events`, not from the notification |
| Concurrency | Only one worker instance consumes at a time (leader elected via `pg_try_advisory_lock`); the others stand by |

**Idempotency key.** `app.events.id` is `BIGSERIAL`, whereas
`analytics.processed_events.event_id` is `UUID`. Migration `0002` adds a UUID
key to `app.events`, and migration `0003` adds the worker's cursor table:

```sql
-- 0002_events_uid.up.sql
ALTER TABLE app.events ADD COLUMN uid UUID NOT NULL DEFAULT gen_random_uuid();
CREATE UNIQUE INDEX events_uid_key ON app.events (uid);
-- + update app.publish_event(): put `id` and `uid` into the pg_notify payload

-- 0003_event_consumers.up.sql
CREATE TABLE app.event_consumers (
    name          TEXT   PRIMARY KEY,           -- e.g. 'go-worker'
    last_event_id BIGINT NOT NULL DEFAULT 0
);
```

`gen_random_uuid()` is volatile, so on a large table `ADD COLUMN … DEFAULT`
rewrites the table; in that case add the column as nullable, backfill in
batches, then set `NOT NULL`.

---

## 6. Hasura GraphQL Engine

### 6.1 What It Does / Does Not Do

| Does | Does not do |
|------|-------------|
| CRUD for simple entities (tags, categories, reference data) | Recalculation of `account.balance_minor` |
| Read queries against tables and views | CSV/OFX parsing |
| Subscriptions (real time) | Fetching rates from external APIs |
| Role-based permissions and filtering by `org_id` | Analytics aggregation |
| Delegation via Actions / Events | Computing `budget_progress`, `holdings` |

### 6.2 Tracked Objects

| Schema | Object | Mode | Note |
|--------|--------|------|------|
| `identity` | `organizations` | R/U | |
| `identity` | `users` | R (self) | `password_hash` is hidden |
| `identity` | `roles`, `user_roles` | R | |
| `identity` | `refresh_tokens` | — | **not tracked** (Go only) |
| `fx` | `currencies` | R | reference table |
| `fx` | `rates` | R | written only from Go |
| `account` | `accounts` | CRUD | deletion via `deleted_at` |
| `account` | `v_active_accounts` | R | view |
| `account` | `balance_ledger` | R | written only from Go |
| `tx` | `categories` | CRUD | |
| `tx` | `transactions` | R, U (`category_id`, `description` only) | creation, deletion and changes to amount/account/kind via Actions |
| `tx` | `v_transactions_full` | R | view |
| `tx` | `tags`, `transaction_tags` | CRUD | |
| `budget` | `budgets`, `budget_limits` | CRUD | |
| `budget` | `budget_progress` | R | written only from Go |
| `invest` | `portfolios`, `assets` | CRUD / R | |
| `invest` | `holdings` | R | written only from Go |
| `invest` | `trades` | R | written via Action |
| `invest` | `price_history` | R | written from cron |
| `import` | `import_jobs` | R, I | started via Action |
| `import` | `raw_rows` | R | |
| `analytics` | `daily_category_summary` | R | |
| `analytics` | `net_worth_snapshots` | R | |
| `app` | `events` | R (admin) | debugging/audit |

### 6.3 Roles and Permissions

Roles are defined in `identity.roles`: `owner`, `admin`, `member`, `viewer`.

| Role | Permissions |
|------|-------------|
| `owner` | Full access, organization management, analytics, import |
| `admin` | CRUD for accounts/transactions, budgets, import |
| `member` | CRUD for transactions, read access to budgets and analytics |
| `viewer` | Read-only |

Example rule (metadata fragment):

```yaml
table: tx.transactions
permissions:
  - role: member
    select:
      filter: { org_id: { _eq: "X-Hasura-Org-Id" } }
    update:
      filter: { org_id: { _eq: "X-Hasura-Org-Id" } }
      columns: [category_id, description]
    # No delete permission, and `deleted_at` is not an updatable column:
    # deletion goes through the `deleteTransaction` Action, which also
    # reverses the balance (see 8.2).
```

**Defense in depth.** A `BEFORE UPDATE` trigger on `tx.transactions` rejects
changes to `amount_minor`, `currency`, `account_id`, `to_account_id`, `kind`
and `deleted_at` unless the session variable `app.via_action` is set (Go sets
it with `SET LOCAL app.via_action = 'on'` inside its own transactions;
Hasura's own queries never do). This keeps balances consistent even if a
Hasura permission is misconfigured later.

### 6.4 Actions (synchronous business logic in Go)

| Action | Type | Endpoint | Purpose |
|--------|------|----------|---------|
| `register` | Mutation | `/actions/auth/register` | Organization + user + system categories |
| `login` | Mutation | `/actions/auth/login` | JWT + refresh |
| `refreshToken` | Mutation | `/actions/auth/refresh` | Token rotation |
| `createAccount` | Mutation | `/actions/account/create` | Validation, initial balance |
| `createTransaction` | Mutation | `/actions/tx/create` | Validation + balance + ledger |
| `createTransfer` | Mutation | `/actions/tx/transfer` | Two-sided transfer |
| `updateTransaction` | Mutation | `/actions/tx/update` | Change amount/account/kind + balance correction |
| `deleteTransaction` | Mutation | `/actions/tx/delete` | Soft delete + reversal of balance and ledger |
| `createTrade` | Mutation | `/actions/invest/trade` | Trade → recalculation of `holdings` |
| `startImport` | Mutation | `/actions/import/start` | File upload, parsing launch |
| `fetchFxRates` | Mutation | `/actions/fx/fetch` | Manual rate request |
| `rebuildAnalytics` | Mutation | `/actions/analytics/rebuild` | Aggregate rebuild |

### 6.5 Events (asynchronous webhooks)

| Event | Table | Condition | Endpoint |
|-------|-------|-----------|----------|
| `transaction_inserted` | `tx.transactions` | INSERT | `/events/tx/created` |
| `transaction_updated` | `tx.transactions` | UPDATE | `/events/tx/updated` |
| `trade_inserted` | `invest.trades` | INSERT | `/events/invest/trade` |
| `import_job_updated` | `import.import_jobs` | UPDATE | `/events/import/job` |
| `fx_rate_inserted` | `fx.rates` | INSERT | `/events/fx/rate` |

### 6.6 Cron Triggers

| Name | Schedule | Action |
|------|----------|--------|
| `fetch_fx_rates` | `0 6 * * *` | Load daily rates → `fx.rates` |
| `rebuild_daily_summary` | `30 0 * * *` | Rebuild `analytics.daily_category_summary` |
| `snapshot_net_worth` | `0 1 * * *` | Snapshot into `analytics.net_worth_snapshots` |
| `rotate_budget_progress` | `0 0 * * 1` | Reset `budget_progress` for the new period |
| `cleanup_tokens` | `0 3 * * 0` | Delete expired `refresh_tokens` |

### 6.7 GraphQL Examples

```graphql
# Active accounts with balance
query ActiveAccounts {
  account_v_active_accounts(order_by: { name: asc }) {
    id
    name
    type
    currency
    balance_minor
  }
}

# Full transaction selection
query Transactions($from: timestamptz!, $to: timestamptz!) {
  tx_v_transactions_full(
    where: { occurred_at: { _gte: $from, _lte: $to } }
    order_by: { occurred_at: desc }
    limit: 50
  ) {
    id
    kind
    amount_minor
    currency
    description
    occurred_at
    account_name
    category_name
    to_account_name
  }
}

# Creating a transaction via Action
mutation CreateTx($input: CreateTransactionInput!) {
  createTransaction(input: $input) {
    transaction_id
    new_balance_minor
    fx_applied
  }
}

# Real-time balance update
subscription Balance($accountId: uuid!) {
  account_accounts_by_pk(id: $accountId) {
    balance_minor
    updated_at
  }
}
```

#### Action Schemas

`Bigint` is a custom scalar carried as a decimal string (`"12345"`); Go parses
it with `strconv.ParseInt` (or `big.Int`). Hasura runs with
`HASURA_GRAPHQL_STRINGIFY_NUMERIC_TYPES=true`, so `bigint`/`numeric` columns
such as `balance_minor` are returned as strings too.

```graphql
# 64-bit signed integer serialized as a decimal string
scalar Bigint

input CreateTransactionInput {
  account_id: uuid!
  category_id: uuid
  kind: TransactionKind!          # income | expense
  amount_minor: Bigint!           # > 0
  currency: String!               # ISO 4217 / crypto
  description: String
  occurred_at: timestamptz!
}

input CreateTransferInput {
  from_account_id: uuid!
  to_account_id: uuid!
  amount_minor: Bigint!
  currency: String!
  description: String
  occurred_at: timestamptz!
}

input CreateTradeInput {
  portfolio_id: uuid!
  asset_id: uuid!
  side: TradeSide!                # buy | sell
  quantity: numeric!
  price_minor: Bigint!
  executed_at: timestamptz!
}

input StartImportInput {
  account_id: uuid!
  file_base64: String!
  file_name: String!
  file_format: ImportFormat!      # csv | ofx
}

input UpdateTransactionInput {
  transaction_id: uuid!
  account_id: uuid
  kind: TransactionKind
  amount_minor: Bigint
  currency: String
  occurred_at: timestamptz
}

input DeleteTransactionInput {
  transaction_id: uuid!
}

enum TransactionKind { income expense transfer }
enum TradeSide { buy sell }
enum ImportFormat { csv ofx }

type CreateTransactionOutput { transaction_id: uuid!, new_balance_minor: Bigint!, fx_applied: Boolean! }
type CreateTradeOutput { holding_id: uuid!, quantity: numeric!, avg_cost_minor: Bigint! }
type TransactionMutationOutput { transaction_id: uuid!, new_balance_minor: Bigint! }
type StartImportOutput { job_id: uuid!, total_rows: Int! }
```

---

## 7. Go Business-Logic Service

### 7.1 Project Structure

```
financeapp-backend/
├── cmd/
│   └── server/
│       └── main.go                    # router + LISTEN worker
├── internal/
│   ├── config/                        # env, flags, validation
│   ├── db/                            # pgx pool, transactions (40001 retry), migrations
│   │   ├── pool.go
│   │   └── tx.go
│   ├── events/                        # LISTEN app_events
│   │   ├── listener.go
│   │   └── dispatcher.go
│   ├── auth/                          # registration, login, refresh
│   │   ├── handler.go
│   │   ├── jwt.go
│   │   └── password.go
│   ├── accounts/                      # accounts, archiving
│   │   └── handler.go
│   ├── transactions/                  # CRUD, transfers, balance
│   │   ├── handler.go
│   │   ├── balance.go
│   │   └── transfer.go
│   ├── budgets/                       # limits and progress
│   │   ├── handler.go
│   │   └── progress.go
│   ├── invest/                        # trades, holdings, valuation
│   │   ├── handler.go
│   │   ├── trade.go
│   │   └── valuation.go
│   ├── imports/                       # parsing, deduplication, mapping
│   │   ├── handler.go
│   │   ├── parser_csv.go
│   │   ├── parser_ofx.go
│   │   ├── dedup.go
│   │   └── mapper.go
│   ├── fx/                            # currency rates
│   │   ├── handler.go
│   │   └── fetcher.go
│   ├── analytics/                     # aggregations, dashboards
│   │   ├── handler.go
│   │   ├── daily_summary.go
│   │   └── net_worth.go
│   └── common/
│       ├── errors.go
│       ├── middleware.go
│       └── money.go                   # minor units respecting minor_unit
├── graphql/
│   └── actions.graphql                # Action definitions for Hasura
├── migrations/
│   ├── 0001_init_schema.up.sql        # = db_schema.sql
│   ├── 0002_events_uid.up.sql         # app.events.uid UUID
│   ├── 0003_event_consumers.up.sql    # app.event_consumers (worker cursor)
│   └── ...
├── Dockerfile
├── docker-compose.yml
└── Makefile
```

### 7.2 Mapping Domains to Packages

| Domain | Package | Area of responsibility |
|--------|---------|------------------------|
| Identity | `auth` | Registration, login, refresh, JWT |
| Accounts | `accounts`, `transactions` | Accounts, `balance_ledger`, balance |
| Transactions | `transactions` | Transactions, transfers, FX conversion |
| Budgets | `budgets` | Limits, `budget_progress`, alerts |
| Invest | `invest` | Trades, `holdings`, valuation via `price_history` |
| Import | `imports` | Parsing, deduplication, mapping |
| FX | `fx` | Loading rates |
| Analytics | `analytics` | `daily_category_summary`, `net_worth_snapshots` |
| Events | `events` | LISTEN worker, idempotency |

---

## 8. Business Logic Details

### 8.0 Transaction Conventions

Every `SERIALIZABLE` operation (8.2, 8.3, the snapshots in 8.7) runs through a
helper in `internal/db/tx.go` that retries the **whole** closure — validation
and reads included — on SQLSTATE `40001` (serialization failure) and `40P01`
(deadlock):

```go
func WithSerializable(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
    const maxAttempts = 5
    var err error
    for attempt := 1; attempt <= maxAttempts; attempt++ {
        err = pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
        var pgErr *pgconn.PgError
        if err == nil || !errors.As(err, &pgErr) ||
            (pgErr.Code != "40001" && pgErr.Code != "40P01") {
            return err
        }
        // jittered exponential backoff: ~10 ms, 20 ms, 40 ms, …
        time.Sleep(time.Duration(5<<attempt+rand.IntN(10)) * time.Millisecond)
    }
    return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, err)
}
```

Rules: `fn` may contain only DB work and must be safe to re-run (no email, push
or HTTP calls inside; those happen after `COMMIT`). When the attempts are
exhausted, the Action returns a retryable error (HTTP 409) to the client. The
trade handler (8.5) relies on a `FOR UPDATE` row lock instead and needs no
`SERIALIZABLE`.

### 8.1 Authentication

```
POST /actions/auth/register
  1. BEGIN
  2. INSERT identity.organizations (plan='free', base_currency)
  3. INSERT identity.users (password_hash = bcrypt, status='active')
  4. INSERT identity.user_roles (role_id → 'owner')
  5. INSERT tx.categories — the system set (is_system = true)
  6. COMMIT
  7. Issue JWT (access, 15 min) + refresh (30 days)

POST /actions/auth/login
  1. SELECT user WHERE email = $1 AND status = 'active'
  2. bcrypt.Compare(password_hash, password)
  3. INSERT identity.refresh_tokens (token_hash, expires_at)
  4. Return { access_token, refresh_token, expires_in }

POST /actions/auth/refresh
  1. Find the token by hash, check expires_at and revoked_at IS NULL
  2. UPDATE the old one → revoked_at = now()
  3. Issue a new token pair
```

**JWT claims format for Hasura:**

```json
{
  "sub": "<user-uuid>",
  "https://hasura.io/jwt/claims": {
    "x-hasura-allowed-roles": ["owner", "admin"],
    "x-hasura-default-role": "owner",
    "x-hasura-org-id": "<org-uuid>",
    "x-hasura-user-id": "<user-uuid>"
  },
  "exp": 1760000000
}
```

### 8.2 Transactions and Balance

```
POST /actions/tx/create
  1. Validation:
     • amount_minor > 0
     • kind ∈ {income, expense}
  2. BEGIN (ISOLATION LEVEL SERIALIZABLE, with retry — see 8.0)
  3. If the transaction currency ≠ the account currency:
     • Find the nearest rate in fx.rates
       (base=tx.currency, quote=account.currency, as_of ≤ occurred_at::date)
     • amount_in_account_currency_minor = amount_minor × rate
       (accounting for the minor_unit of both currencies)
  4. INSERT tx.transactions
  5. INSERT account.balance_ledger (reason='transaction', ref_id=tx.id)
     • income  → +amount
     • expense → −amount
  6. UPDATE account.accounts SET balance_minor = balance_minor ± delta
  7. COMMIT (the trigger publishes the event to app.events by itself)
  8. Return { transaction_id, new_balance_minor, fx_applied }

POST /actions/tx/delete
  1. BEGIN SERIALIZABLE (with retry — see 8.0)
     SET LOCAL app.via_action = 'on'
  2. SELECT … FROM tx.transactions WHERE id = $1 FOR UPDATE
     • not found / other org → error
     • deleted_at IS NOT NULL → no-op, return current balance
  3. Lock all affected accounts in ascending id order (FOR UPDATE);
     a transfer affects two
  4. Compute the reversal from account.balance_ledger: the net SUM of
     ledger amounts per account for ref_id = tx.id
     (do NOT recompute from today's FX rates)
  5. INSERT account.balance_ledger — one row per account with the opposite
     sign (reason='adjustment', ref_id = tx.id)
  6. UPDATE account.accounts SET balance_minor = balance_minor + reversal
  7. UPDATE tx.transactions SET deleted_at = now()
  8. COMMIT (the trigger publishes `transaction.deleted`)
  9. Return { transaction_id, new_balance_minor }

POST /actions/tx/update   (amount_minor, currency, account_id, kind, occurred_at)
  1–3. As in delete: lock the transaction and every account involved
       (old and new)
  4. Reverse the old effect exactly as in delete (steps 4–6)
  5. Apply the new values exactly as in create (FX conversion for the new
     occurred_at, new ledger rows, balance update)
  6. UPDATE tx.transactions SET …
  7. COMMIT (the trigger publishes `transaction.updated`; the budget handler
     recalculates from scratch — see 8.4)
```

### 8.3 Transfers

```
POST /actions/tx/transfer
  1. BEGIN SERIALIZABLE (with retry — see 8.0)
  2. Lock accounts in ascending id order (deadlock protection):
     SELECT … FROM account.accounts WHERE id IN (from, to) FOR UPDATE
  3. Convert if the currencies differ
  4. INSERT tx.transactions
     (kind='transfer', to_account_id, category_id=NULL)
  5. INSERT balance_ledger × 2 (reason='transfer', ref_id=tx.id):
     • source: −amount
     • target: +amount_in_target_currency
  6. UPDATE both accounts
  7. COMMIT
```

### 8.4 Budgets

```
Event handler: transaction.created / .updated / .deleted

  1. If kind ≠ 'expense' — exit
  2. Find all budget_limits for the category (including child categories
     if include_subcategories = true):
     WITH RECURSIVE tree AS (
       SELECT id FROM tx.categories WHERE id = $category_id
       UNION ALL
       SELECT c.id FROM tx.categories c JOIN tree t ON c.parent_id = t.id
     )
     SELECT bl.* FROM budget.budget_limits bl
     JOIN tree ON bl.category_id = tree.id
  3. UPSERT budget.budget_progress (PK = budget_limit_id)
     • Determine the current period starting from budget.starts_on
       (monthly / weekly / custom)
     • spent_minor += amount_minor (or recalculate the sum from scratch
       on .updated / .deleted)
  4. If spent_minor / limit_minor ≥ alert_pct/100:
     → app.publish_event('budget_limit', id, 'budget.alert', …)
     → notification (email / push)
  5. Update analytics.daily_category_summary
```

### 8.5 Investments

```
POST /actions/invest/trade
  1. BEGIN
  2. Find or create the position:
     INSERT INTO invest.holdings(portfolio_id, asset_id, quantity, avg_cost_minor)
     VALUES ($1, $2, 0, 0)
     ON CONFLICT (portfolio_id, asset_id) DO NOTHING
  3. SELECT … FROM invest.holdings
     WHERE portfolio_id=$1 AND asset_id=$2 FOR UPDATE
  4. If buy:
     • quantity += trade.quantity
     • avg_cost_minor = (old_qty·old_avg + qty·price) / new_qty
     If sell:
     • Check: quantity ≥ trade.quantity
     • quantity -= trade.quantity
  5. INSERT invest.trades (holding_id, side, quantity, price_minor)
  6. UPDATE invest.holdings
  7. COMMIT
  8. Return the updated holding

Position valuation:
  value_minor = quantity × price_history.close_minor (latest as_of)
```

### 8.6 Import

```
POST /actions/import/start
  1. Save the file (locally / S3)
  2. INSERT import.import_jobs (status='pending')
  3. Asynchronously:
     a. status → 'parsing'
     b. Row-by-row parsing (parsers for csv and ofx)
     c. For each row:
        • dedup_hash = SHA256(account + FITID) when the file carries a bank
          transaction id (OFX); otherwise
          SHA256(account + date + amount + description + occurrence_n),
          where occurrence_n is the ordinal (0, 1, 2, …) of the identical
          row within this file. This way two genuine identical purchases on
          the same day are not collapsed and do not violate
          UNIQUE(import_job_id, dedup_hash)
        • INSERT import.raw_rows (status='pending')
        • total_rows++
     d. status → 'mapping'
     e. For each row:
        • Look up dedup_hash among raw_rows of earlier jobs for the same
          account (status='mapped'); a match → status='duplicate', skip
          the steps below (UNIQUE(import_job_id, dedup_hash) only guards
          within one job)
        • Mapping: account / category →
          INSERT tx.transactions (source='import', import_job_id)
        • UPDATE raw_rows SET status='mapped', mapped_transaction_id
     f. status → 'completed' / 'failed', completed_at = now()
  4. Return { job_id, total_rows }
```

### 8.7 FX and Analytics

```
fetch_fx_rates (cron, daily):
  1. Query the external API for the pairs needed by the organization
  2. INSERT INTO fx.rates … ON CONFLICT (base_code, quote_code, as_of)
     DO UPDATE SET rate = EXCLUDED.rate, source = EXCLUDED.source
  3. The trigger publishes an event → recalculation of multi-currency aggregates

rebuild_daily_summary ($from is a date; one DB transaction, under the same
advisory lock as the event worker's analytics updates; SET LOCAL TIME ZONE 'UTC'):
  1. DELETE FROM analytics.daily_category_summary
     WHERE day >= $from;
     -- removes days whose transactions were all deleted or moved
  2. INSERT INTO analytics.daily_category_summary
       (org_id, category_id, day, total_minor, currency)
     SELECT org_id, category_id, occurred_at::date,
            SUM(amount_minor), currency
     FROM tx.transactions
     WHERE deleted_at IS NULL
       AND kind <> 'transfer'        -- transfers are neither income nor expense
       AND category_id IS NOT NULL   -- (transfers have no category anyway)
       AND occurred_at >= $from
     GROUP BY 1,2,3,5;
     -- a category has a single kind (income | expense), so a sum
     -- never mixes income and expense

snapshot_net_worth:
  Sum of balances of all active accounts
  (in the organization's base currency at the latest rates)
  → INSERT INTO analytics.net_worth_snapshots (org_id, as_of, net_worth_minor, currency)
```

### 8.8 Event Worker

`NOTIFY` is only a wake-up signal. The worker always reads events from
`app.events` by cursor, so lost notifications, truncated payloads, restarts
and reconnects cannot lose events.

```go
// internal/events/listener.go
var errNotLeader = errors.New("another worker instance holds the lock")

const retryDelay = 5 * time.Second

// Run keeps one dedicated LISTEN connection alive and reconnects after
// any failure; every reconnect is followed by a catch-up from the cursor.
func Run(ctx context.Context, dsn string, d *Dispatcher) error {
    for ctx.Err() == nil {
        err := listenOnce(ctx, dsn, d)
        switch {
        case ctx.Err() != nil:
            return ctx.Err()
        case errors.Is(err, errNotLeader):
            slog.Debug("event worker on standby")
        default:
            slog.Error("event listener stopped, reconnecting", "err", err, "retry_in", retryDelay)
        }
        select {
        case <-time.After(retryDelay):
        case <-ctx.Done():
        }
    }
    return ctx.Err()
}

func listenOnce(ctx context.Context, dsn string, d *Dispatcher) error {
    conn, err := pgx.Connect(ctx, dsn) // dedicated connection, not from the pool
    if err != nil {
        return err
    }
    defer conn.Close(context.Background())

    var leader bool
    if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", workerLockID).Scan(&leader); err != nil {
        return err
    }
    if !leader {
        return errNotLeader // the lock is released automatically if this connection dies
    }

    // LISTEN first, then catch up, so nothing can fall into the gap.
    if _, err := conn.Exec(ctx, "LISTEN app_events"); err != nil {
        return err
    }
    for {
        if err := d.CatchUp(ctx); err != nil { // also covers downtime and reconnects
            return err
        }
        if _, err := conn.WaitForNotification(ctx); err != nil { // wake-up only
            return err
        }
    }
}
```

`Dispatcher.CatchUp` drains `app.events` in batches until nothing is left.
One event is processed per DB transaction:

```
processBatch:
  1. cursor := SELECT last_event_id FROM app.event_consumers WHERE name = 'go-worker'
  2. SELECT * FROM app.events
     WHERE id > $cursor - 1000          -- trailing window, see 5.4
     ORDER BY id LIMIT 500
  3. for each event:
       BEGIN
       INSERT INTO analytics.processed_events (event_id) VALUES (evt.uid)
         ON CONFLICT DO NOTHING         -- 0 rows → already handled, skip
       handler(evt)                     -- its writes join this same transaction
       UPDATE app.event_consumers
         SET last_event_id = greatest(last_event_id, evt.id)
         WHERE name = 'go-worker'
       COMMIT
```

**Idempotency:** the `processed_events` row is written in the same
transaction as the handler's effects, so an event is either fully handled and
recorded, or not at all. Side effects outside the database (email, push) run
after `COMMIT` and are therefore at-least-once.

**Failures:** because processing is strictly ordered, a failing handler blocks
the queue. The error is logged, the event is retried on the next pass, and
the `financeapp_events_failed_total` metric raises an alert. After review, an
operator can move `app.event_consumers.last_event_id` past a poison event.

---

## 9. Configuration and Deployment

### 9.1 Environment Variables

```env
# PostgreSQL
# (inside the compose network use sslmode=disable unless Postgres TLS is configured)
DB_PASSWORD=<password>
DATABASE_URL=postgres://app:<password>@postgres:5432/financeapp?sslmode=disable

# Hasura
HASURA_GRAPHQL_ADMIN_SECRET=<secret>
HASURA_GRAPHQL_JWT_SECRET={"type":"RS256","key":"<public-pem>"}
HASURA_GRAPHQL_UNAUTHORIZED_ROLE=anonymous
HASURA_GRAPHQL_STRINGIFY_NUMERIC_TYPES=true   # bigint/numeric as strings (see 6.7)
HASURA_ACTION_BASE_URL=http://backend:9090
HASURA_ACTION_SECRET=<secret>   # sent as X-Action-Secret (value_from_env in Action/Event
                                # metadata); verified by the Go middleware

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

### 9.2 docker-compose.yml

Values come from a `.env` file that is not committed (see 9.1).

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: financeapp
      POSTGRES_USER: app
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./db_schema.sql:/docker-entrypoint-initdb.d/01_schema.sql:ro
    # No published port: only hasura and backend reach Postgres over the
    # compose network. For local debugging use "127.0.0.1:5432:5432".
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app -d financeapp"]
      interval: 5s
      timeout: 3s
      retries: 10

  hasura:
    image: hasura/graphql-engine:v2.45.0
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      HASURA_GRAPHQL_DATABASE_URL: ${DATABASE_URL}
      HASURA_GRAPHQL_ADMIN_SECRET: ${HASURA_GRAPHQL_ADMIN_SECRET}
      HASURA_GRAPHQL_JWT_SECRET: ${HASURA_GRAPHQL_JWT_SECRET}
      HASURA_GRAPHQL_UNAUTHORIZED_ROLE: anonymous
      HASURA_GRAPHQL_STRINGIFY_NUMERIC_TYPES: "true"
      HASURA_ACTION_BASE_URL: http://backend:9090
      HASURA_ACTION_SECRET: ${HASURA_ACTION_SECRET}
    ports: ["8080:8080"]

  backend:
    build: .
    depends_on:
      postgres:
        condition: service_healthy
      hasura:
        condition: service_started
    environment:
      DATABASE_URL: ${DATABASE_URL}
      LISTEN_ADDR: ":9090"
      HASURA_ACTION_SECRET: ${HASURA_ACTION_SECRET}
    # Not published: the Action handlers trust the session variables Hasura
    # forwards, so they must be reachable only over the compose network.
    expose: ["9090"]

volumes:
  pgdata:
```

(The obsolete top-level `version:` key has been removed; Compose ignores it
and warns about it.)

### 9.3 Migrations

```bash
make migrate-create name=<name>   # new migration
make migrate-up                   # apply
make migrate-down                 # roll back
```

- `0001` — the contents of `db_schema.sql`.
- `0002` — `app.events.uid UUID` (idempotency key).
- `0003` — `app.event_consumers` (event worker cursor).
- then — category seed data, indexes as the data grows.

### 9.4 CI/CD

```
push → CI
  1. golangci-lint
  2. go test -race -cover
  3. docker build
  4. migrations on staging
  5. integration tests (testcontainers + Hasura)
  6. docker push
  7. rollout to prod
```

---

## 10. Observability

| Layer | Tool |
|-------|------|
| Logs | `slog` → stdout → Loki |
| Metrics | `prometheus/client_golang` → Grafana |
| Traces | OpenTelemetry → Jaeger |
| Audit/debugging | `app.events` — full event history |
| Errors | Sentry |

Key metrics:

```
financeapp_http_requests_total{handler, method, status}
financeapp_http_request_duration_seconds{handler}
financeapp_events_processed_total{event_type}
financeapp_events_processing_duration_seconds{event_type}
financeapp_events_failed_total{event_type}
financeapp_events_lag            # max(app.events.id) − last_event_id
financeapp_import_rows_total{job_id, status}
financeapp_fx_fetch_duration_seconds
financeapp_db_pool_in_use
```

---

## 11. Security

| Threat | Mitigation |
|--------|-----------|
| SQL injection | Parameterized queries only (`pgx`) |
| Data access | JWT + Hasura permission sets by `org_id` |
| Password leakage | bcrypt (cost ≥ 12) |
| Token theft | Short-lived access token + refresh rotation |
| Secrets | Only via environment variables / Vault |
| Transport | TLS on all endpoints |
| Data isolation | Enable RLS from block 14 of the schema for multi-tenancy |
| Forged Action/Event calls | Backend ports are not published; handlers also verify the shared `X-Action-Secret` header sent by Hasura |
| Bypassing business logic via GraphQL | Narrow Hasura permissions + the `app.via_action` guard trigger (see 6.3) |

---

## 12. Scaling and Multi-Tenancy

### 12.1 Current State (personal application)

- One user, one organization.
- RLS is disabled (the commented-out block 14 in the schema).

### 12.2 Enabling Multi-Tenancy

1. Uncomment block 14 in `db_schema.sql` for every table with `org_id`:
   ```sql
   ALTER TABLE account.accounts ENABLE ROW LEVEL SECURITY;
   CREATE POLICY accounts_tenant_isolation ON account.accounts
       USING (org_id = current_setting('app.current_org')::UUID);
   ```
2. In Go, before each query, execute:
   ```sql
   SET LOCAL app.current_org = '<uuid>';
   ```
3. Hasura already forwards `X-Hasura-Org-Id` from the JWT — filtering is preserved.

### 12.3 Horizontal Scaling

| Load | Strategy |
|------|----------|
| Reads | PostgreSQL read replica for Hasura |
| Writes | A single primary; Go services behind a load balancer |
| Events | Several instances may run, but only one consumes at a time (advisory-lock leader, see 8.8); a standby takes over automatically; idempotency via `processed_events`. To scale consumption further, partition by `aggregate_id` with one cursor per partition |
| Analytics | Move to a separate worker / materialized views |
| Moving to microservices | Replace `LISTEN/NOTIFY` with Kafka; `app.events` → an outbox per service |

---

## 13. Summary by Layer

```
┌─────────────────────────────────────────────────────────────────────┐
│  HASURA                                                             │
│  ✓ GraphQL CRUD for simple entities (tags, categories, accounts)    │
│  ✓ Subscriptions (real-time balances, transactions)                 │
│  ✓ Role model (owner / admin / member / viewer)                     │
│  ✗ DOES NOT do: balance recalculation, parsing, aggregations        │
├─────────────────────────────────────────────────────────────────────┤
│  GO SERVICE (via Actions / Events / Cron / LISTEN)                  │
│  ✓ Authentication: registration, login, refresh-token rotation      │
│  ✓ Transactions + recalc of account.balance_minor, balance_ledger   │
│  ✓ Transfers (two legs, currency conversion)                        │
│  ✓ Recalculation of budget.budget_progress and alerts               │
│  ✓ Trades → recalculation of invest.holdings                        │
│  ✓ CSV/OFX parsing, deduplication, mapping                          │
│  ✓ Loading fx.rates, aggregating analytics.*                        │
│  ✓ Processing events from app_events (LISTEN)                       │
├─────────────────────────────────────────────────────────────────────┤
│  POSTGRESQL                                                         │
│  ✓ Single source of truth, real FKs                                 │
│  ✓ Transactional integrity (CHECK, UNIQUE, SERIALIZABLE)            │
│  ✓ Triggers → app.events + NOTIFY ('app_events')                    │
│  ✓ Event log for audit and analytics rebuilding                     │
└─────────────────────────────────────────────────────────────────────┘
```

---

## Appendix A. Revision Notes (rev. 2)

| # | Problem in rev. 1 | Fix in rev. 2 | Sections |
|---|-------------------|---------------|----------|
| 1 | GraphQL `Int` (32-bit) used for `BIGINT` amounts | Custom `Bigint` scalar carried as a string; Hasura stringifies numeric types; note on the `BIGINT` range | 1, 6.7, 9.1, 9.2 |
| 2 | `member` could soft-delete / delete transactions directly through Hasura and bypass balance recalculation | Permissions narrowed; new `updateTransaction` / `deleteTransaction` Actions with exact ledger reversal; guard trigger | 6.2–6.4, 8.2 |
| 3 | Worker had no real reconnect and no catch-up, contrary to the stated guarantees | `NOTIFY` as wake-up only, cursor table, reconnect loop, advisory-lock leader, per-event transaction | 5.4, 8.8, 9.3, 12.3 |
| 4 | `rebuild_daily_summary` summed transfers with income/expense and left stale rows | Transfers excluded; delete-then-insert for the rebuilt range | 8.7 |
| 5 | `dedup_hash` collapsed identical purchases (and would violate the `UNIQUE` constraint within one file); cross-job duplicates were not detected | FITID / occurrence index in the hash; lookup against earlier jobs | 3.2, 8.6 |
| 6 | No retry for `SERIALIZABLE` failures | `WithSerializable` helper retrying on `40001` / `40P01` | 8.0 |
| 7 | `BIGSERIAL` vs `UUID` mismatch only mentioned in a note | Migrations `0002` / `0003` spelled out | 5.4, 9.3 |
| 8 | docker-compose: obsolete `version`, DB port published, duplicated hard-coded connection strings, no health check, backend port published | Fixed; connection string taken from `.env`; `service_healthy` dependency; backend not published; shared secret between Hasura and Go | 9.1, 9.2, 11 |

**Open decision.** The schema stores `*_minor` as `BIGINT`, which cannot hold
more than ≈ 9.22 ETH at 18 decimals (see the note in section 1). Either
switch the affected columns to `NUMERIC(38,0)` or cap the stored precision.

---

*End of document*