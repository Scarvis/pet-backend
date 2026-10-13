// Event dispatch (§8.8, §5.4): CatchUp drains app.events in batches by cursor.
// One event is processed per DB transaction: the processed_events row, the
// handler's writes and the cursor update commit atomically, so an event is
// either fully handled and recorded, or not at all. Side effects outside the
// database (budget alert notifications) run after COMMIT and are therefore
// at-least-once.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/budgets"
	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/db"
)

// Consumer names the cursor row this worker advances in app.event_consumers.
const consumerName = "go-worker"

// Batch bounds for one catch-up pass (§8.8 processBatch): the trailing window
// re-scans already-seen ids (BIGSERIAL values can become visible out of order
// under concurrent transactions) and processed_events filters repeats (§5.4).
const (
	trailingWindow = 1000
	batchLimit     = 500
)

// Dispatcher routes consumed events to their domain handlers. Notify receives
// budget alerts after each event's COMMIT (at-least-once); when nil, alerts are
// logged. OnEventFailed is the financeapp_events_failed_total hook (§10):
// it fires once per poison event before CatchUp aborts the pass.
// OnEventProcessed is the financeapp_events_processed_total /
// financeapp_events_processing_duration_seconds hook (§10): it fires once per
// successfully handled event with its type and DB-transaction duration.
type Dispatcher struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	Notify           func(ctx context.Context, alerts []budgets.Alert)
	OnEventFailed    func(eventType string)
	OnEventProcessed func(eventType string, d time.Duration)
}

// NewDispatcher builds a Dispatcher over pool.
func NewDispatcher(pool *pgxpool.Pool, log *slog.Logger) *Dispatcher {
	return &Dispatcher{pool: pool, log: log}
}

// event is one app.events row. uid is the idempotency key recorded in
// analytics.processed_events; payload is the full row JSON (the worker reads
// full rows, never the possibly-truncated NOTIFY payload, §5.4).
type event struct {
	id        int64
	uid       uuid.UUID
	eventType string
	payload   []byte
}

// transactionPayload carries the fields HandleTransactionEvent needs. The
// trigger stores to_jsonb(NEW), so soft-deleted rows still carry kind and
// category_id from their pre-delete state.
type transactionPayload struct {
	OrgID      uuid.UUID  `json:"org_id"`
	CategoryID *uuid.UUID `json:"category_id"`
	Kind       string     `json:"kind"`
	OccurredAt time.Time  `json:"occurred_at"`
}

// parseTransactionPayload extracts routing fields from an event payload.
// Pure function — unit-tested without a database.
func parseTransactionPayload(raw []byte) (transactionPayload, error) {
	var p transactionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return transactionPayload{}, common.Wrap(err)
	}
	if p.OrgID == uuid.Nil {
		return transactionPayload{}, common.Wrap(common.BadRequest("transaction event payload misses org_id"))
	}
	return p, nil
}

// ensureConsumer creates the cursor table/row when the deployment predates
// migration 0003 (which ARCHITECTURE §5.4 assumes but the schema has not
// applied everywhere yet). Idempotent and safe to run on every pass.
func ensureConsumer(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS app.event_consumers (name TEXT PRIMARY KEY, last_event_id BIGINT NOT NULL DEFAULT 0)`)
	if err != nil {
		return common.Wrap(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO app.event_consumers (name, last_event_id) VALUES ($1, 0) ON CONFLICT (name) DO NOTHING`, consumerName)
	return common.Wrap(err)
}

// CatchUp drains app.events until nothing is left after the cursor. Processing
// is strictly ordered by ascending id; a failing handler aborts the pass (the
// error is logged, the failure hook fires, the event is retried on the next
// pass) and an operator can move the cursor past a poison event (§8.8).
func (d *Dispatcher) CatchUp(ctx context.Context) error {
	if err := ensureConsumer(ctx, d.pool); err != nil {
		return err
	}
	for {
		more, err := d.processBatch(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// processBatch handles up to batchLimit events past the cursor. It reports
// whether a full batch was seen (another pass may be needed).
func (d *Dispatcher) processBatch(ctx context.Context) (bool, error) {
	var cursor int64
	err := d.pool.QueryRow(ctx, `SELECT last_event_id FROM app.event_consumers WHERE name = $1`, consumerName).Scan(&cursor)
	if err != nil {
		return false, common.Wrap(err)
	}

	rows, err := d.pool.Query(ctx, `SELECT id, uid, event_type, payload FROM app.events WHERE id > GREATEST($1 - $2, 0) ORDER BY id LIMIT $3`,
		cursor, trailingWindow, batchLimit)
	if err != nil {
		return false, common.Wrap(err)
	}
	var batch []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.id, &e.uid, &e.eventType, &e.payload); err != nil {
			rows.Close()
			return false, common.Wrap(err)
		}
		batch = append(batch, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, common.Wrap(err)
	}
	if len(batch) == 0 {
		return false, nil
	}

	for _, e := range batch {
		if err := d.processOne(ctx, e); err != nil {
			d.log.Error("event processing failed, queue blocked", "event_id", e.id, "uid", e.uid, "type", e.eventType, "err", err)
			if d.OnEventFailed != nil {
				d.OnEventFailed(e.eventType)
			}
			return false, err
		}
	}
	return len(batch) == batchLimit, nil
}

// processOne handles a single event in one SERIALIZABLE transaction and sends
// its notifications only after COMMIT. Only DB work runs inside the closure
// (§8.0); alerts are captured and delivered afterwards.
func (d *Dispatcher) processOne(ctx context.Context, e event) error {
	var alerts []budgets.Alert
	start := time.Now()
	err := db.WithSerializable(ctx, d.pool, func(tx pgx.Tx) error {
		alerts = nil // reset on retry so notifications never duplicate
		tag, err := tx.Exec(ctx, `INSERT INTO analytics.processed_events (event_id, topic) VALUES ($1, $2) ON CONFLICT DO NOTHING`, e.uid, e.eventType)
		if err != nil {
			return common.Wrap(err)
		}
		if tag.RowsAffected() == 0 {
			// Already handled: skip the handler but still advance the cursor
			// below so the trailing window drains.
		} else if a, err := d.route(ctx, tx, e); err != nil {
			return err
		} else {
			alerts = a
		}
		_, err = tx.Exec(ctx, `UPDATE app.event_consumers SET last_event_id = GREATEST(last_event_id, $2) WHERE name = $1`, consumerName, e.id)
		return common.Wrap(err)
	})
	if err != nil {
		return err
	}
	if d.OnEventProcessed != nil {
		d.OnEventProcessed(e.eventType, time.Since(start))
	}
	d.deliver(ctx, alerts)
	return nil
}

// route calls the domain handler for e; its writes join tx. It returns the
// post-COMMIT notifications collected for this event.
func (d *Dispatcher) route(ctx context.Context, tx pgx.Tx, e event) ([]budgets.Alert, error) {
	switch e.eventType {
	case "transaction.created", "transaction.updated", "transaction.deleted":
		p, err := parseTransactionPayload(e.payload)
		if err != nil {
			return nil, err
		}
		alerts, err := budgets.HandleTransactionEvent(ctx, tx, p.OrgID, p.CategoryID, p.Kind, p.OccurredAt)
		if err != nil {
			return nil, err
		}
		if err := refreshDailySummaryTx(ctx, tx, p.OrgID, p.OccurredAt); err != nil {
			return nil, err
		}
		return alerts, nil
	case "fx_rate.created", "fx_rate.deleted":
		// Multi-currency aggregate recalculation rides on the analytics
		// rebuild path (cron/manual); the rate row itself is already stored.
		d.log.Debug("fx rate event consumed", "event_id", e.id, "type", e.eventType)
		return nil, nil
	case "trade.created", "trade.updated", "trade.deleted",
		"price_history.created", "price_history.deleted",
		"import_job.created", "import_job.updated", "import_job.deleted":
		// Holdings are recalculated synchronously by the trade Action (§8.5)
		// and imports run their own pipeline (§8.6); the worker only records
		// idempotency for these today.
		d.log.Debug("event consumed, no worker-side effects", "event_id", e.id, "type", e.eventType)
		return nil, nil
	default:
		// Unknown aggregates (account, category, budget, …): audit-only.
		d.log.Debug("unrouted event consumed", "event_id", e.id, "type", e.eventType)
		return nil, nil
	}
}

// refreshDailySummaryTx re-aggregates one org/day cell of
// analytics.daily_category_summary inside the worker's per-event transaction
// (§8.4 step 5). It mirrors rebuild_daily_summary (§8.7) scoped to the day the
// event touched: transfers and NULL categories are excluded.
func refreshDailySummaryTx(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, occurredAt time.Time) error {
	day := occurredAt.UTC().Truncate(24 * time.Hour)
	if _, err := tx.Exec(ctx, `DELETE FROM analytics.daily_category_summary WHERE org_id = $1 AND day = $2::date`, orgID, day); err != nil {
		return common.Wrap(err)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO analytics.daily_category_summary (org_id, category_id, day, total_minor, currency)
		SELECT org_id, category_id, occurred_at::date, SUM(amount_minor), currency
		FROM tx.transactions
		WHERE deleted_at IS NULL
		  AND kind <> 'transfer'
		  AND category_id IS NOT NULL
		  AND org_id = $1
		  AND occurred_at::date = $2::date
		GROUP BY 1, 2, 3, 5`, orgID, day)
	return common.Wrap(err)
}

// deliver sends post-COMMIT notifications (at-least-once, §8.8). The default is
// structured logging; Phase 8 wires the real email/push sender via Notify.
func (d *Dispatcher) deliver(ctx context.Context, alerts []budgets.Alert) {
	if len(alerts) == 0 {
		return
	}
	if d.Notify != nil {
		d.Notify(ctx, alerts)
		return
	}
	for _, a := range alerts {
		d.log.Info("budget alert", "budget_limit_id", a.BudgetLimitID, "budget_id", a.BudgetID,
			"spent_minor", a.SpentMinor, "limit_minor", a.LimitMinor, "alert_pct", a.AlertPct)
	}
}
