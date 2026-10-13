# events — LISTEN Worker & Dispatcher

At-least-once event consumption over `app.events` + `pg_notify('app_events')`. See `ARCHITECTURE.md` §5.4 and §8.8.

## Entry points

- `Run(ctx, dsn, d)` / `listenOnce` (`listener.go`) — long-lived worker started from `cmd/server`. Dedicated `pgx.Connect` (not the pool), advisory-lock leader election (`pg_try_advisory_lock`, key `FINAPP`), `LISTEN app_events` **first**, then catch-up loop. Reconnects after any failure with 5s delay; standby instances exit with `errNotLeader`.
- `Dispatcher{pool, log, Notify, OnEventFailed, OnEventProcessed}` + `NewDispatcher(pool, log)` (`dispatcher.go`).
- `CatchUp(ctx)` — drains `app.events` in `id` order until the cursor is exhausted (`processBatch`, 500 rows/batch).

Hasura event webhooks (`/events/tx/created`, `/tx/updated`, `/invest/trade`, `/import/job`, `/fx/rate`) only ack and rely on `CatchUp` for real work.

## Tables

`app.events` (`id BIGSERIAL` order, `uid UUID` idempotency key), `app.event_consumers(name='go-worker', last_event_id)` — self-bootstrapped with `CREATE TABLE IF NOT EXISTS` (absent from real `0003` SQL), `analytics.processed_events(event_id)` (idempotency), plus budget/analytics tables via `budgets.HandleTransactionEvent` and `refreshDailySummaryTx`.

## Rules

- One event per `SERIALIZABLE` tx: `INSERT processed_events … ON CONFLICT DO NOTHING` (0 rows → already handled, skip but still advance cursor) + handler writes + `UPDATE event_consumers SET last_event_id = greatest(…, evt.id)`.
- Strict `id ASC` processing; a failing handler aborts the pass, logs, fires `OnEventFailed` (→ `financeapp_events_failed_total`), and is retried next pass. Operator moves the cursor past poison events.
- Trailing-window re-scan (`cursor − 1000`, computed in Go to avoid `$1−$2` typing errors) handles `BIGSERIAL` out-of-order visibility; `processed_events` filters repeats.
- Routing: `transaction.{created,updated,deleted}` → budget recalc + single-cell daily-summary refresh; `fx_rate.*` / `trade.*` / `price_history.*` / `import_job.*` → idempotency-only (holdings sync lives in the trade Action, imports own their pipeline); everything else → audit-only.
- Full rows read from `app.events`, never the (possibly truncated) NOTIFY payload. Side effects (email/push via `Notify []budgets.Alert`) run after `COMMIT` — at-least-once.

## Hooks / metrics

`Notify` (default: log), `OnEventProcessed` (→ `processed_total` + duration), `OnEventFailed` (→ `failed_total`). Lag gauge = `max(app.events.id) − last_event_id` (see `observability`).

## Tests

`dispatcher_test.go` — `parseTransactionPayload` (requires `org_id`, allows null `category_id`). Worker paths need a DB.
