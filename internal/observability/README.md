# observability — Prometheus Metrics

`financeapp_*` metrics (§10) and HTTP instrumentation. Wired in `cmd/server/main.go` (`/metrics` via promhttp, per-handler `Instrument`, 15s lag/pool ticker).

## Metrics (`metrics.go`)

| Metric | Type | Labels |
|---|---|---|
| `financeapp_http_requests_total` | Counter | `handler, method, status` |
| `financeapp_http_request_duration_seconds` | Histogram | `handler` |
| `financeapp_events_processed_total` | Counter | `event_type` |
| `financeapp_events_processing_duration_seconds` | Histogram | `event_type` |
| `financeapp_events_failed_total` | Counter | `event_type` |
| `financeapp_events_lag` | Gauge | — (`max(app.events.id) − last_event_id`) |
| `financeapp_import_rows_total` | Counter | `job_id, status` |
| `financeapp_fx_fetch_duration_seconds` | Histogram | — |
| `financeapp_db_pool_in_use` | Gauge | — |

## Helpers

`Instrument(name, next)`, `ObserveEventProcessed(type, dur)` / `ObserveEventFailed(type)` (consume `events.Dispatcher` hooks), `ObserveImportRows(job, status, n)`, `ObserveFXFetch(dur)`, `SetEventsLag(n)`, `SetDBPoolInUse(n)`.

Logs (`slog` → stdout → Loki), traces (OpenTelemetry → Jaeger), and errors (Sentry) are configured at the deployment layer, not here.
