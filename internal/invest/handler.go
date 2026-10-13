// Package invest implements trades, holdings and position valuation (§7.1,
// §8.5). Trade execution lives in trade.go, valuation in valuation.go; this
// file holds the Handler and portfolio/holding reads.
package invest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/config"
)

// Handler serves the invest Actions. It owns the db pool and logger;
// cmd/server wires it to HTTP endpoints (§7.1).
type Handler struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewHandler builds an invest Handler.
func NewHandler(pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{pool: pool, log: log}
}

// Portfolio is one invest.portfolios row (§schema).
type Portfolio struct {
	ID        uuid.UUID `json:"id"`
	OrgID     uuid.UUID `json:"org_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Holding is one invest.holdings row. Quantity is NUMERIC(28,10) carried as a
// decimal string so precision survives the wire (§6.7 convention).
type Holding struct {
	ID           uuid.UUID `json:"holding_id"`
	PortfolioID  uuid.UUID `json:"portfolio_id"`
	AssetID      uuid.UUID `json:"asset_id"`
	Quantity     string    `json:"quantity"`
	AvgCostMinor int64     `json:"avg_cost_minor"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CreatePortfolio inserts a portfolio for the acting org.
func (h *Handler) CreatePortfolio(ctx context.Context, name string) (*Portfolio, error) {
	if name == "" {
		return nil, common.Wrap(common.BadRequest("name must not be empty"))
	}
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	var p Portfolio
	err = h.pool.QueryRow(ctx, `INSERT INTO invest.portfolios (org_id, name) VALUES ($1, $2) RETURNING id, org_id, name, created_at`,
		orgID, name).Scan(&p.ID, &p.OrgID, &p.Name, &p.CreatedAt)
	if err != nil {
		return nil, common.Wrap(err)
	}
	h.log.Info("portfolio created", "id", p.ID, "name", name)
	return &p, nil
}

// ListPortfolios returns all portfolios of the acting org.
func (h *Handler) ListPortfolios(ctx context.Context) ([]Portfolio, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, `SELECT id, org_id, name, created_at FROM invest.portfolios WHERE org_id = $1 ORDER BY created_at ASC`, orgID)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()
	var out []Portfolio
	for rows.Next() {
		var p Portfolio
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.CreatedAt); err != nil {
			return nil, common.Wrap(err)
		}
		out = append(out, p)
	}
	return out, common.Wrap(rows.Err())
}

// DeletePortfolio removes a portfolio; holdings and trades cascade (§schema).
func (h *Handler) DeletePortfolio(ctx context.Context, id uuid.UUID) error {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return err
	}
	tag, err := h.pool.Exec(ctx, `DELETE FROM invest.portfolios WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return common.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return common.Wrap(common.NotFound("portfolio not found"))
	}
	h.log.Info("portfolio deleted", "id", id)
	return nil
}

// ListHoldings returns all positions of one portfolio (org-scoped).
func (h *Handler) ListHoldings(ctx context.Context, portfolioID uuid.UUID) ([]Holding, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, `SELECT h.id, h.portfolio_id, h.asset_id, h.quantity::text, h.avg_cost_minor, h.updated_at FROM invest.holdings h JOIN invest.portfolios p ON p.id = h.portfolio_id WHERE h.portfolio_id = $1 AND p.org_id = $2 ORDER BY h.id`, portfolioID, orgID)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()
	var out []Holding
	for rows.Next() {
		var hd Holding
		if err := rows.Scan(&hd.ID, &hd.PortfolioID, &hd.AssetID, &hd.Quantity, &hd.AvgCostMinor, &hd.UpdatedAt); err != nil {
			return nil, common.Wrap(err)
		}
		out = append(out, hd)
	}
	return out, common.Wrap(rows.Err())
}

// GetHolding returns one position by id, or NotFound outside the acting org.
func (h *Handler) GetHolding(ctx context.Context, id uuid.UUID) (*Holding, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	var hd Holding
	err = h.pool.QueryRow(ctx, `SELECT h.id, h.portfolio_id, h.asset_id, h.quantity::text, h.avg_cost_minor, h.updated_at FROM invest.holdings h JOIN invest.portfolios p ON p.id = h.portfolio_id WHERE h.id = $1 AND p.org_id = $2`, id, orgID).
		Scan(&hd.ID, &hd.PortfolioID, &hd.AssetID, &hd.Quantity, &hd.AvgCostMinor, &hd.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, common.Wrap(common.NotFound("holding not found"))
	}
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &hd, nil
}
