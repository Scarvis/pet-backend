package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxAttempts is how many times WithSerializable retries a closure that fails
// with a serialization conflict or a deadlock (§8.0).
const maxAttempts = 5

// WithSerializable runs fn inside a SERIALIZABLE transaction, retrying the whole
// closure (validation and reads included) when PostgreSQL reports a serialization
// failure (SQLSTATE 40001) or a deadlock (40P01). Between attempts it sleeps for
// a jittered exponential backoff (~5 ms base: ~5, 10, 20, … milliseconds).
//
// The closure must contain only database work and be safe to re-run; side effects
// such as email or push notifications belong after COMMIT. When the attempts are
// exhausted it returns a retryable error so the caller can surface HTTP 409.
func WithSerializable(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)

		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) ||
			(pgErr.Code != "40001" && pgErr.Code != "40P01") {
			return err
		}

		time.Sleep(time.Duration(5<<attempt+rand.IntN(10)) * time.Millisecond)
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, err)
}
