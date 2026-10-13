// Package analytics serves read-model maintenance: the rebuildAnalytics Action
// (§6.4) and the cron jobs rebuild_daily_summary / snapshot_net_worth (§6.6).
// The transactional work lives in daily_summary.go and net_worth.go; this file
// holds the Handler wiring them to HTTP endpoints (Phase 8).
package analytics

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/config"
)

// Handler serves the analytics Actions. It owns the db pool and logger;
// cmd/server wires it to HTTP endpoints (§7.1).
type Handler struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewHandler builds an analytics Handler.
func NewHandler(pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{pool: pool, log: log}
}

// Rebuild regenerates analytics.daily_category_summary for every day >= from
// (§8.7). It backs the rebuildAnalytics Action (§6.4).
func (h *Handler) Rebuild(ctx context.Context, from time.Time) error {
	if err := RebuildDailySummary(ctx, h.pool, from); err != nil {
		return err
	}
	h.log.Info("daily summary rebuilt", "from", from.UTC().Truncate(24*time.Hour))
	return nil
}

// SnapshotNetWorth snapshots the acting org's net worth as of asOf (§8.7).
func (h *Handler) SnapshotNetWorth(ctx context.Context, asOf time.Time) error {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return err
	}
	return SnapshotNetWorth(ctx, h.pool, orgID, asOf)
}

// SnapshotOrg snapshots one explicit org (used by the cron job, §6.6, which
// has no acting user in context).
func (h *Handler) SnapshotOrg(ctx context.Context, orgID uuid.UUID, asOf time.Time) error {
	return SnapshotNetWorth(ctx, h.pool, orgID, asOf)
}
