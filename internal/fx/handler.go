// Package fx loads daily currency rates (§8.7 fetch_fx_rates): the cron job (or
// the fetchFxRates Action, §6.4) calls FetchDailyRates, which pulls every base
// currency from the external API and upserts the rows. The fx.rates trigger
// publishes the fx_rate event itself, so the handler only inserts rows.
package fx

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/config"
	"github.com/Scarvis/pet-backend/internal/db"
)

// Handler serves the FX cron/Action endpoints. It owns the db pool, the logger
// and the external-API credentials; cmd/server wires it to HTTP (§7.1).
type Handler struct {
	pool   *pgxpool.Pool
	log    *slog.Logger
	apiURL string
	apiKey string
	client *http.Client
}

// NewHandler builds an fx Handler. Credentials come from FX_API_URL/FX_API_KEY
// (§9.1); client may be nil, in which case a default 30s-timeout client is used.
func NewHandler(pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{
		pool:   pool,
		log:    log,
		apiURL: cfg.FXAPIURL,
		apiKey: cfg.FXAPIKey,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// FetchDailyRates loads the daily rates for every base in bases for asOf
// (§8.7 steps 1–2). HTTP fetching happens before the transaction; the upserts
// run in one SERIALIZABLE transaction so a daily load is atomic. It returns
// the number of rows written. A partial failure aborts the whole load with an
// error so the miss is visible instead of silently half-loaded.
func (h *Handler) FetchDailyRates(ctx context.Context, bases []string, asOf time.Time) (int, error) {
	if len(bases) == 0 {
		return 0, common.Wrap(common.BadRequest("at least one base currency is required"))
	}
	day := asOf.UTC().Truncate(24 * time.Hour)

	var all []Rate
	for _, base := range bases {
		rates, err := FetchBaseRates(ctx, h.client, h.apiURL, h.apiKey, base, day)
		if err != nil {
			return 0, err
		}
		all = append(all, rates...)
	}

	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		return UpsertRatesTx(ctx, tx, all)
	})
	if err != nil {
		return 0, err
	}
	h.log.Info("fx rates loaded", "bases", len(bases), "rows", len(all), "as_of", day.Format("2006-01-02"))
	return len(all), nil
}
