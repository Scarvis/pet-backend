// Command server is the Go business-logic service entrypoint
// (ARCHITECTURE.md §7.1): it builds the chi router for all Hasura Actions
// (§6.4), Events (§6.5) and Cron webhooks (§6.6), exposes Prometheus metrics
// (§10), and runs the LISTEN event worker (events.Run, §8.8) with graceful
// shutdown on SIGINT/SIGTERM.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Scarvis/pet-backend/internal/accounts"
	"github.com/Scarvis/pet-backend/internal/analytics"
	"github.com/Scarvis/pet-backend/internal/auth"
	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/config"
	"github.com/Scarvis/pet-backend/internal/db"
	"github.com/Scarvis/pet-backend/internal/events"
	"github.com/Scarvis/pet-backend/internal/fx"
	"github.com/Scarvis/pet-backend/internal/imports"
	"github.com/Scarvis/pet-backend/internal/invest"
	"github.com/Scarvis/pet-backend/internal/observability"
	"github.com/Scarvis/pet-backend/internal/transactions"
)

const maxBodyBytes = 8 << 20 // 8 MiB; import files arrive base64-encoded

// server bundles every domain handler so route closures stay thin.
type server struct {
	pool      *pgxpool.Pool
	auth      *auth.Handler
	accounts  *accounts.Handler
	tx        *transactions.Handler
	invest    *invest.Handler
	imports   *imports.Handler
	fx        *fx.Handler
	analytics *analytics.Handler
	dispatch  *events.Dispatcher
}

// actionEnvelope is the Hasura Action request body: the Action arguments under
// "input" plus the session variables Hasura forwards from the JWT.
type actionEnvelope struct {
	Input            json.RawMessage   `json:"input"`
	SessionVariables map[string]string `json:"session_variables"`
}

// decodeInput reads the request body and unmarshals the Action arguments into
// dst. Hasura wraps arguments in {"input": {...}}; manual cron callers may
// POST the raw object, which is accepted as a fallback. It returns the
// Hasura session variables for identity injection.
func decodeInput(r *http.Request, dst any) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, common.BadRequest("cannot read request body")
	}
	if int64(len(body)) > maxBodyBytes {
		return nil, common.BadRequest("request body too large")
	}
	var env actionEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, common.BadRequest("invalid JSON body")
	}
	raw := env.Input
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = body // raw-object fallback for manual cron calls
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return nil, common.BadRequest("invalid action input: " + err.Error())
	}
	return env.SessionVariables, nil
}

// withIdentity injects the acting user into ctx from the X-Hasura-User-ID
// header (preferred) or the Hasura session variables (§7.1). When neither
// carries a user id the context is returned unchanged and the handler
// reports a missing identity.
func withIdentity(ctx context.Context, r *http.Request, sessionVars map[string]string) context.Context {
	raw := r.Header.Get("X-Hasura-User-Id")
	if raw == "" {
		for k, v := range sessionVars {
			if strings.EqualFold(k, "x-hasura-user-id") {
				raw = v
				break
			}
		}
	}
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || id == uuid.Nil {
		return ctx
	}
	return common.WithUserID(ctx, id)
}

// handle decodes a Hasura Action body into Req, injects the identity, runs fn
// and renders the result (or a JSON error via the common writer).
func handle[Req any, Res any](w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, req Req) (Res, error)) {
	var req Req
	sessionVars, err := decodeInput(r, &req)
	if err != nil {
		common.WriteError(w, err)
		return
	}
	ctx := withIdentity(r.Context(), r, sessionVars)
	res, err := fn(ctx, req)
	if err != nil {
		common.WriteError(w, err)
		return
	}
	common.WriteJSON(w, res)
}

func toBigint(v int64) common.Bigint { return common.Bigint(strconv.FormatInt(v, 10)) }

// parseNumeric accepts a GraphQL numeric sent either as a JSON string or as a
// JSON number and returns its decimal rendering.
func parseNumeric(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", common.BadRequest("quantity is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "", common.BadRequest("quantity is required")
		}
		return strings.TrimSpace(s), nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", common.BadRequest("quantity must be a decimal string or number")
	}
	return n.String(), nil
}

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	s := &server{
		pool:      pool,
		auth:      auth.NewHandler(pool, cfg, log),
		accounts:  accounts.NewHandler(pool, cfg, log),
		tx:        transactions.NewHandler(pool, cfg, log),
		invest:    invest.NewHandler(pool, cfg, log),
		imports:   imports.NewHandler(pool, cfg, log),
		fx:        fx.NewHandler(pool, cfg, log),
		analytics: analytics.NewHandler(pool, cfg, log),
		dispatch:  events.NewDispatcher(pool, log),
	}

	// §10: dispatcher → Prometheus hooks.
	s.dispatch.OnEventFailed = observability.ObserveEventFailed
	s.dispatch.OnEventProcessed = observability.ObserveEventProcessed

	// LISTEN worker (§8.8) runs until shutdown; standby/error loops inside Run.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go func() {
		if err := events.Run(workerCtx, cfg.DatabaseURL, s.dispatch); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("event worker stopped", "err", err)
		}
	}()

	// §10: lag + pool gauges refreshed on a short ticker.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-t.C:
				observeRuntime(workerCtx, pool, log)
			}
		}
	}()
	observeRuntime(ctx, pool, log)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           buildRouter(s, cfg, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}

	cancelWorker()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutCtx)
}

// observeRuntime refreshes the events-lag and db-pool gauges (§10). Failures
// are logged at debug level: metrics must never break request handling.
func observeRuntime(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	observability.SetDBPoolInUse(float64(pool.Stat().AcquiredConns()))
	var lag int64
	err := pool.QueryRow(ctx, `SELECT (SELECT COALESCE(MAX(id),0) FROM app.events) - (SELECT COALESCE(last_event_id,0) FROM app.event_consumers WHERE name='go-worker')`).Scan(&lag)
	if err != nil {
		log.Debug("events lag query failed", "err", err)
		return
	}
	if lag < 0 {
		lag = 0
	}
	observability.SetEventsLag(float64(lag))
}

// buildRouter assembles the chi router: public /healthz + /metrics and the
// secret-checked Actions/Events/Cron tree (§6.4–6.6). Every protected route is
// instrumented with its handler name for §10 http metrics.
func buildRouter(s *server, cfg *config.Config, log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		common.WriteJSON(w, map[string]string{"status": "ok"})
	})
	r.Handle("/metrics", promhttp.Handler())

	mw := common.NewMiddleware(s.pool, cfg.HasuraActionSecret, log)
	r.Group(func(r chi.Router) {
		r.Use(mw.Handler)
		registerActionRoutes(r, s)
		registerEventRoutes(r, s)
		registerCronRoutes(r, s)
	})
	return r
}

// post instruments name and registers a POST route on the protected router.
func post(r chi.Router, name, pattern string, h http.HandlerFunc) {
	r.Post(pattern, observability.Instrument(name, h).ServeHTTP)
}
