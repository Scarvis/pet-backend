# Database Schemas — pet-db

PostgreSQL 16+ monolith for a personal finance app. All schemas live in one database with real foreign keys.
Migrations run in order: `db_schema_0001.sql` (base) → `db_schema_0002.sql` → `db_schema_0003.sql`.

Global conventions:

- **Money:** `BIGINT` minor units (cents), never float/decimal. Currency codes are `CHAR(3)` FK → `fx.currencies(code)`.
- **`updated_at`:** auto-maintained only on tables with the `app.set_updated_at()` trigger (organizations, users, accounts, transactions, budget_progress, holdings). Other tables do not self-update.
- **Soft delete:** `deleted_at TIMESTAMPTZ` on `identity.users`, `account.accounts`, `tx.transactions`. Other tables use hard deletes.
- **RLS:** disabled. Opt-in multi-tenancy via `SET LOCAL app.current_org = '<uuid>'` (see commented block at end of 0001).
- **New constraints on existing data:** two-step `ADD CONSTRAINT ... NOT VALID` + `VALIDATE CONSTRAINT`.

## `app` — events + utilities

Single event journal replacing Kafka/outbox. Workers subscribe with `LISTEN app_events`.

| Object                                                                               | Purpose                                                                                                                                                                                                                                                                                                              |
| ------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `app.events(id, uid, aggregate_type, aggregate_id, event_type, payload, created_at)` | Source of truth for domain events. `uid UUID` (added in 0003) is the idempotency key; `id BIGSERIAL` is internal ordering. Indexes on `created_at`, `event_type`, `(aggregate_type, aggregate_id)`, unique on `uid`.                                                                                                 |
| `app.publish_event(type, id, event, payload)`                                        | Inserts into `app.events`, returns `uid` in the NOTIFY payload. If payload > ~7000 bytes, sends `{"truncated": true, "payload": {}}` — consumer must re-read full row by `uid`.                                                                                                                                      |
| `app.event_notify()`                                                                 | Generic trigger: arg1 = aggregate name, arg2 = `soft`/`hard`, optional arg3 = comma-separated key columns for composite keys. `soft` requires a `deleted_at` column (0003 errors otherwise). Emits `<aggregate>.created/.updated/.deleted`; soft-delete (`deleted_at` NULL → NOT NULL on UPDATE) maps to `.deleted`. |
| `app.set_updated_at()`                                                               | `BEFORE UPDATE` trigger setting `NEW.updated_at = now()`.                                                                                                                                                                                                                                                            |

`aggregate_id` contract (0002/0003): scalar `id::TEXT` (UUID or BIGINT-as-text), or JSON object for composite keys. Only case today: `invest.price_history` → `{"asset_id":"…","as_of":"…"}`. Consumers parse: try UUID → try JSON (`{` prefix) → else plain text.

Idempotency: consumers check `analytics.processed_events(event_id = uid)` before processing. No FK `processed_events → events.uid` by design, so journal rotation won't break it.

## `identity` — users, orgs, roles

| Table                                                                                                                                                             | Key columns                                                                 |
| ----------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| `organizations(id UUID, name, plan free/pro/team, base_currency → fx.currencies, timestamps)`                                                                     | Tenant root. Almost every business table FKs here with `ON DELETE CASCADE`. |
| `users(id, org_id → organizations, email CITEXT UNIQUE, password_hash, display_name, status active/suspended/deleted, email_verified_at, timestamps, deleted_at)` | `CITEXT` email = case-insensitive unique. Event trigger `user/soft`.        |
| `roles(id SMALLSERIAL, code UNIQUE)`                                                                                                                              | Seed: `owner, admin, member, viewer`.                                       |
| `user_roles(user_id, role_id)`                                                                                                                                    | PK `(user_id, role_id)`.                                                    |
| `refresh_tokens(id, user_id, token_hash UNIQUE, expires_at, revoked_at, created_at)`                                                                              | Partial index on `user_id WHERE revoked_at IS NULL`.                        |

## `fx` — currencies and rates

| Table                                                                                                                       | Key columns                                                                                                                                                       |
| --------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `currencies(code CHAR(3) PK, name, minor_unit)`                                                                             | Seed: USD, EUR, RUB, GBP, JPY (0), CHF, CNY, KZT, UAH, BTC (8), ETH (18). Referenced by all money `currency` columns (FKs added `NOT VALID` + validated in 0002). |
| `rates(id BIGSERIAL, base_code → currencies, quote_code → currencies, rate NUMERIC(20,10), as_of DATE, source, created_at)` | `UNIQUE(base, quote, as_of)` + lookup index. Event trigger `fx_rate/hard` on INSERT/DELETE only (no UPDATE trigger).                                              |

## `account` — accounts and balance ledger

| Table                                                                                                                                                                | Key columns                                                                                                |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `accounts(id, org_id, owner_user_id → users, name, type cash/card/deposit/debt, currency → fx, balance_minor BIGINT DEFAULT 0, is_archived, timestamps, deleted_at, is_hidden BOOLEAN NOT NULL DEFAULT false)` | Event trigger `account/soft`. Indexes on `org_id`, `owner_user_id`.                                        |
| `balance_ledger(id BIGSERIAL, account_id → accounts CASCADE, delta_minor, reason transaction/adjustment/transfer, ref_id → tx.transactions SET NULL, created_at)`    | Append-only movement history behind `balance_minor`. Event trigger `balance_ledger/hard` on INSERT/DELETE. |

View `account.v_active_accounts`: non-deleted, non-archived accounts with balance.

## `tx` — categories, transactions, tags

Core write path.

| Table                                                                                                                                                                                                                                                                                                                                             | Key columns                                                                                                                                                                                                                                                                                                                                  |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `categories(id, org_id, parent_id → self SET NULL, name, kind income/expense, icon, is_system, created_at)`                                                                                                                                                                                                                                       | Self-referencing tree. Uniqueness `UNIQUE NULLS NOT DISTINCT (org_id, parent_id, name)` (fixed in 0002 so two root categories can't share a name). System categories are seeded per-org at signup (`is_system = true`).                                                                                                                      |
| `transactions(id, org_id, account_id → accounts, to_account_id → accounts NULL, category_id → categories SET NULL, kind income/expense/transfer, amount_minor > 0, currency → fx, amount_in_account_currency_minor NULL, description, occurred_at, source manual/import/recurring, import_job_id → import_jobs SET NULL, timestamps, deleted_at)` | Transfer invariant: `kind='transfer'` requires `to_account_id NOT NULL`, `category_id IS NULL`, `account_id <> to_account_id`. `import_job_id` FK added after `import.import_jobs` creation (deferred creation order). Indexes: `(org_id, occurred_at)`, `(account_id, occurred_at)`, `(category_id, occurred_at) WHERE deleted_at IS NULL`. |
| `tags(id, org_id, name)`                                                                                                                                                                                                                                                                                                                          | `UNIQUE(org_id, name)`.                                                                                                                                                                                                                                                                                                                      |
| `transaction_tags(transaction_id CASCADE, tag_id CASCADE)`                                                                                                                                                                                                                                                                                        | PK `(transaction_id, tag_id)`.                                                                                                                                                                                                                                                                                                               |

View `tx.v_transactions_full`: live transactions with `account_name/currency`, `category_name`, `to_account_name`.

## `budget` — budgets and limits

| Table                                                                                                                                                      | Key columns                                   |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------- |
| `budgets(id, org_id, name, period monthly/weekly/custom, starts_on DATE, created_at)`                                                                      | Event trigger `budget/hard`.                  |
| `budget_limits(id, budget_id CASCADE, category_id → categories, limit_minor > 0, currency → fx, include_subcategories DEFAULT true, alert_pct DEFAULT 80)` | `UNIQUE(budget_id, category_id)`.             |
| `budget_progress(budget_limit_id PK → limits CASCADE, spent_minor, period_start, period_end, updated_at)`                                                  | 1:1 with limit; has `set_updated_at` trigger. |

## `invest` — portfolios and market data

| Table                                                                                                         | Key columns                                                                                                                                                                      |
| ------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `portfolios(id, org_id, name, created_at)`                                                                    | Event trigger `portfolio/hard`.                                                                                                                                                  |
| `assets(id, symbol, asset_type stock/crypto/deposit/bond, currency → fx)`                                     | `UNIQUE(symbol, asset_type)`. Global catalog (no `org_id`).                                                                                                                      |
| `holdings(id, portfolio_id CASCADE, asset_id, quantity NUMERIC(28,10) DEFAULT 0, avg_cost_minor, updated_at)` | `UNIQUE(portfolio_id, asset_id)`. `CHECK (quantity >= 0)` added in 0003 (`NOT VALID` + validate) — no shorts at DB level.                                                        |
| `trades(id, holding_id CASCADE, side buy/sell, quantity > 0, price_minor, executed_at, created_at)`           | Index `(holding_id, executed_at)`.                                                                                                                                               |
| `price_history(asset_id CASCADE, as_of DATE, close_minor, PK(asset_id, as_of))`                               | Composite key. Event trigger uses composite-key form `event_notify('price_history','hard','asset_id,as_of')` (0002) — the only JSON `aggregate_id` producer. INSERT/DELETE only. |

## `import` — CSV/OFX ingestion

| Table                                                                                                                                                                                                                 | Key columns                                               |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| `import_jobs(id, org_id, account_id → accounts, file_name, file_format csv/ofx, status pending/parsing/mapping/completed/failed, total_rows, imported_rows, duplicate_rows, error_message, created_at, completed_at)` | Referenced by `tx.transactions.import_job_id`.            |
| `raw_rows(id BIGSERIAL, import_job_id CASCADE, row_number, raw_data JSONB, dedup_hash, status pending/mapped/duplicate/error, mapped_transaction_id → transactions SET NULL)`                                         | `UNIQUE(import_job_id, dedup_hash)` = dedup within a job. |

Flow: `import_jobs` row → `raw_rows` per line → dedup on `dedup_hash` → mapped rows create `tx.transactions` with `source='import'`.

## `analytics` — read models

Rebuilt from `app.events`; never written by domain triggers directly.

| Table                                                                                                                               | Key columns                                                                                                                                                                                         |
| ----------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `daily_category_summary(org_id CASCADE, category_id, day DATE, total_minor, currency → fx, PK(org_id, category_id, day, currency))` | Dashboard rollup.                                                                                                                                                                                   |
| `net_worth_snapshots(org_id CASCADE, as_of DATE, net_worth_minor, currency → fx, PK(org_id, as_of))`                                | Point-in-time net worth.                                                                                                                                                                            |
| `processed_events(event_id UUID PK, topic, processed_at)`                                                                           | Idempotency log; `event_id = app.events.uid`. Deliberately no FK (see `app`). Worker pattern documented at end of 0003: `SELECT` → skip if present → process → `INSERT ... ON CONFLICT DO NOTHING`. |

## Triggers and views

Event triggers (all `AFTER ... FOR EACH ROW`): `users(user/soft)`, `accounts(account/soft)`, `balance_ledger(balance_ledger/hard, no UPDATE)`, `categories(category/hard)`, `transactions(transaction/soft)`, `tags(tag/hard)`, `budgets`, `budget_limits`, `portfolios`, `holdings`, `trades`, `price_history(price_history/hard + composite key, no UPDATE)`, `import_jobs`, `raw_rows`, `fx.rates(fx_rate/hard, no UPDATE)`.

`set_updated_at` (`BEFORE UPDATE`): organizations, users, accounts, transactions, budget_progress, holdings.

Views: `tx.v_transactions_full`, `account.v_active_accounts` (both filter `deleted_at IS NULL`).

## Migration history

- **0001:** base schema — 9 schemas, all tables, event/publish functions (single-`id` only), seed roles + currencies, commented RLS block.
- **0002:** `event_notify` rewritten for composite keys (JSON `aggregate_id`) and strict key validation; `price_history` trigger recreated with `asset_id,as_of`; category uniqueness fixed to `NULLS NOT DISTINCT`; 7 currency FKs added `NOT VALID` + validated.
- **0003:** `app.events.uid UUID` + unique index; `publish_event` returns `uid` in NOTIFY; index `(aggregate_type, aggregate_id)`; `event_notify` rejects `soft` on tables without `deleted_at`; `holdings.quantity >= 0` check (`NOT VALID` + validate); documents worker idempotency pattern (no FK to `uid`).
