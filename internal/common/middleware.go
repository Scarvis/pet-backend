package common

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnProvider returns a dedicated connection for the request. *pgxpool.Pool
// satisfies it via Acquire, so db.NewPool's pool can be passed straight in.
type ConnProvider interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// Middleware verifies the Hasura X-Action-Secret header (§9.1, §11) and
// recovers panics into JSON HTTP errors instead of crashing the process.
//
// Note on `app.via_action` (§6.3): the guard trigger only accepts the flag via
// `SET LOCAL` inside a transaction, on the same connection that runs the
// UPDATE. A middleware-level connection cannot provide that (handlers check
// out their own connections from the pool), so each mutating handler sets
// `SET LOCAL app.via_action = 'on'` inside its own transaction (see e.g. the
// transaction update/delete paths). The pool is retained for health checks.
type Middleware struct {
	pool   ConnProvider
	secret string
	logger *slog.Logger
}

// NewMiddleware builds middleware for the given pool, shared Hasura action secret
// and logger. An empty secret disables verification (useful in local dev).
func NewMiddleware(pool ConnProvider, secret string, logger *slog.Logger) *Middleware {
	return &Middleware{pool: pool, secret: secret, logger: logger}
}

// Handler returns an http.Handler that applies the middleware to next. It works
// with any chi router because chi.Router implements http.Handler.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.checkSecret(r) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid X-Action-Secret")
			return
		}

		defer m.recover(w)
		next.ServeHTTP(w, r)
	})
}

// checkSecret verifies the X-Action-Secret header matches the shared secret.
func (m *Middleware) checkSecret(r *http.Request) bool {
	if m.secret == "" {
		return true // verification disabled
	}
	got := r.Header.Get("X-Action-Secret")
	return got != "" && got == m.secret
}

// recover turns a panic from the wrapped handler into a JSON HTTP error instead
// of crashing the process. A typed *Error carries its own status and message;
// any other panic value is logged and rendered as 500.
func (m *Middleware) recover(w http.ResponseWriter) {
	defer func() {
		if rec := recover(); rec != nil {
			var eErr *Error
			switch v := rec.(type) {
			case *Error:
				eErr = v
			default:
				m.logger.Error("panic recovered from Action handler", "value", rec)
				eErr = New(http.StatusInternalServerError, "internal_error", "internal server error")
			}
			if eErr != nil {
				writeJSONError(w, eErr.Status, eErr.Code, eErr.Message)
			}
		}
	}()
}
