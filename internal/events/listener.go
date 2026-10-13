// Event LISTEN worker transport (§8.8): the reconnect loop, advisory-lock
// leader election and LISTEN-first catch-up. NOTIFY is only a wake-up signal;
// every reconnect is followed by a catch-up from the cursor, so lost
// notifications, truncated payloads, restarts and reconnects cannot lose events.
package events

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// errNotLeader reports that another worker instance holds the advisory lock;
// the instance stays on standby and retries instead of consuming.
var errNotLeader = errors.New("another worker instance holds the lock")

// retryDelay is the pause before reconnecting after any listener failure.
const retryDelay = 5 * time.Second

// workerLockID is the advisory-lock key electing the single consuming worker
// (§5.4 concurrency). It must be stable across restarts and instances.
const workerLockID int64 = 0x46494e415050 // "FINAPP" in hex

// Run keeps one dedicated LISTEN connection alive and reconnects after any
// failure; every reconnect is followed by a catch-up from the cursor.
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
