// Package budgets implements budget and budget-limit CRUD (see ARCHITECTURE.md
// §7.1). Progress recalculation lives in progress.go (§8.4); this file holds
// only the synchronous Action handlers. Every mutating method runs inside a
// SERIALIZABLE transaction with retry so concurrent transaction events cannot
// interleave a limit change with a progress upsert.
package budgets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/config"
	"github.com/Scarvis/pet-backend/internal/db"
)

// Periods are the CHECK-constraint values for budget.period (§3.2).
var budgetPeriods = map[string]bool{"monthly": true, "weekly": true, "custom": true}

// Handler serves the budget Actions. It owns the db pool and logger;
// cmd/server wires it to HTTP endpoints (§7.1).
type Handler struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewHandler builds a budgets Handler.
func NewHandler(pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{pool: pool, log: log}
}

// Budget is one budget.budgets row (§schema).
type Budget struct {
	ID        uuid.UUID `json:"id"`
	OrgID     uuid.UUID `json:"org_id"`
	Name      string    `json:"name"`
	Period    string    `json:"period"` // monthly | weekly | custom
	StartsOn  time.Time `json:"starts_on"`
	CreatedAt time.Time `json:"created_at"`
}

// BudgetLimit is one budget.budget_limits row (§schema).
type BudgetLimit struct {
	ID                   uuid.UUID `json:"id"`
	BudgetID             uuid.UUID `json:"budget_id"`
	CategoryID           uuid.UUID `json:"category_id"`
	LimitMinor           int64     `json:"limit_minor"`
	Currency             string    `json:"currency"`
	IncludeSubcategories bool      `json:"include_subcategories"`
	AlertPct             int       `json:"alert_pct"`
}

// BudgetProgress is one budget.budget_progress row (§schema). PeriodEnd is a
// real DATE (NOT NULL): custom budgets use the 9999-12-31 sentinel from
// PeriodBounds as their open-ended marker.
type BudgetProgress struct {
	BudgetLimitID uuid.UUID `json:"budget_limit_id"`
	SpentMinor    int64     `json:"spent_minor"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// BudgetPlan is one budget.budget_plans row — an explicit budget plan value
// per (period_type, period_start, org, ref).
type BudgetPlan struct {
	ID        uuid.UUID `json:"id"`
	OrgID     uuid.UUID `json:"org_id"`
	PeriodType string    `json:"period_type"` // month | quarter | year
	PeriodStart time.Time `json:"period_start"`
	RefType   string    `json:"ref_type"`    // category | account
	CategoryID uuid.UUID `json:"category_id"` // exactly one, per ref_type
	AccountID uuid.UUID `json:"account_id"`
	AmountMinor int64    `json:"amount_minor"`
	Currency  string    `json:"currency"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func validatePeriod(period string) error {
	if !budgetPeriods[period] {
		return common.BadRequest("period must be one of monthly, weekly, custom")
	}
	return nil
}

func validateLimit(limitMinor int64, alertPct int) error {
	if limitMinor <= 0 {
		return common.BadRequest("limit_minor must be a positive integer")
	}
	if alertPct <= 0 || alertPct > 100 {
		return common.BadRequest("alert_pct must be between 1 and 100")
	}
	return nil
}

// CreateBudget inserts a new budget for the acting org.
func (h *Handler) CreateBudget(ctx context.Context, name, period string, startsOn time.Time) (*Budget, error) {
	if name == "" {
		return nil, common.Wrap(common.BadRequest("name must not be empty"))
	}
	if err := validatePeriod(period); err != nil {
		return nil, common.Wrap(err)
	}

	var b Budget
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		orgID, err := common.ActingOrg(ctx, h.pool)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `INSERT INTO budget.budgets (org_id, name, period, starts_on) VALUES ($1, $2, $3, $4) RETURNING id, org_id, name, period, starts_on, created_at`,
			orgID, name, period, startsOn)
		return row.Scan(&b.ID, &b.OrgID, &b.Name, &b.Period, &b.StartsOn, &b.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("budget created", "id", b.ID, "name", name)
	return &b, nil
}

// ListBudgets returns all budgets of the acting org.
func (h *Handler) ListBudgets(ctx context.Context) ([]Budget, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, `SELECT id, org_id, name, period, starts_on, created_at FROM budget.budgets WHERE org_id = $1 ORDER BY created_at ASC`, orgID)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()
	var out []Budget
	for rows.Next() {
		var b Budget
		if err := rows.Scan(&b.ID, &b.OrgID, &b.Name, &b.Period, &b.StartsOn, &b.CreatedAt); err != nil {
			return nil, common.Wrap(err)
		}
		out = append(out, b)
	}
	return out, common.Wrap(rows.Err())
}

// GetBudget returns one budget by id, or NotFound when it does not exist or
// belongs to another org.
func (h *Handler) GetBudget(ctx context.Context, id uuid.UUID) (*Budget, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	var b Budget
	err = h.pool.QueryRow(ctx, `SELECT id, org_id, name, period, starts_on, created_at FROM budget.budgets WHERE id = $1 AND org_id = $2`, id, orgID).
		Scan(&b.ID, &b.OrgID, &b.Name, &b.Period, &b.StartsOn, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, common.Wrap(common.NotFound("budget not found"))
	}
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &b, nil
}

// UpdateBudget changes name/period/starts_on. Nil pointers leave the field
// untouched. Changing the period re-anchors future progress recalculation
// (§8.4 step 3); existing progress rows are refreshed on the next transaction
// event rather than rewritten here.
func (h *Handler) UpdateBudget(ctx context.Context, id uuid.UUID, name, period *string, startsOn *time.Time) (*Budget, error) {
	if period != nil {
		if err := validatePeriod(*period); err != nil {
			return nil, common.Wrap(err)
		}
	}
	if name != nil && *name == "" {
		return nil, common.Wrap(common.BadRequest("name must not be empty"))
	}

	var b Budget
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		orgID, err := common.ActingOrg(ctx, h.pool)
		if err != nil {
			return err
		}
		var cur Budget
		err = tx.QueryRow(ctx, `SELECT id, org_id, name, period, starts_on, created_at FROM budget.budgets WHERE id = $1 AND org_id = $2 FOR UPDATE`, id, orgID).
			Scan(&cur.ID, &cur.OrgID, &cur.Name, &cur.Period, &cur.StartsOn, &cur.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("budget not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if name != nil {
			cur.Name = *name
		}
		if period != nil {
			cur.Period = *period
		}
		if startsOn != nil {
			cur.StartsOn = *startsOn
		}
		err = tx.QueryRow(ctx, `UPDATE budget.budgets SET name = $2, period = $3, starts_on = $4 WHERE id = $1 RETURNING id, org_id, name, period, starts_on, created_at`,
			cur.ID, cur.Name, cur.Period, cur.StartsOn).
			Scan(&b.ID, &b.OrgID, &b.Name, &b.Period, &b.StartsOn, &b.CreatedAt)
		return common.Wrap(err)
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("budget updated", "id", b.ID)
	return &b, nil
}

// DeleteBudget removes a budget; budget_limits cascade and their progress rows
// cascade from the limits (§schema FK chain).
func (h *Handler) DeleteBudget(ctx context.Context, id uuid.UUID) error {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return err
	}
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM budget.budgets WHERE id = $1 AND org_id = $2`, id, orgID)
		if err != nil {
			return common.Wrap(err)
		}
		if tag.RowsAffected() == 0 {
			return common.Wrap(common.NotFound("budget not found"))
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.log.Info("budget deleted", "id", id)
	return nil
}

// CreateLimit attaches a per-category spending limit to a budget. The budget and
// the category must both belong to the acting org; UNIQUE(budget_id,
// category_id) rejects a second limit for the same category (§3.2).
func (h *Handler) CreateLimit(ctx context.Context, budgetID, categoryID uuid.UUID, limitMinor int64, currency string, includeSubcategories bool, alertPct int) (*BudgetLimit, error) {
	if err := validateLimit(limitMinor, alertPct); err != nil {
		return nil, common.Wrap(err)
	}
	if !common.ValidCurrency(ctx, h.pool, currency) {
		return nil, common.Wrap(common.BadRequest(fmt.Sprintf("unknown currency %q", currency)))
	}

	var l BudgetLimit
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		orgID, err := common.ActingOrg(ctx, h.pool)
		if err != nil {
			return err
		}
		var budgetOrg uuid.UUID
		err = tx.QueryRow(ctx, `SELECT org_id FROM budget.budgets WHERE id = $1`, budgetID).Scan(&budgetOrg)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("budget not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if budgetOrg != orgID {
			return common.Wrap(common.NotFound("budget not found"))
		}
		var catOrg uuid.UUID
		err = tx.QueryRow(ctx, `SELECT org_id FROM tx.categories WHERE id = $1`, categoryID).Scan(&catOrg)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("category not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if catOrg != orgID {
			return common.Wrap(common.NotFound("category not found"))
		}
		err = tx.QueryRow(ctx, `INSERT INTO budget.budget_limits (budget_id, category_id, limit_minor, currency, include_subcategories, alert_pct) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, budget_id, category_id, limit_minor, currency, include_subcategories, alert_pct`,
			budgetID, categoryID, limitMinor, currency, includeSubcategories, alertPct).
			Scan(&l.ID, &l.BudgetID, &l.CategoryID, &l.LimitMinor, &l.Currency, &l.IncludeSubcategories, &l.AlertPct)
		return common.Wrap(err)
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("budget limit created", "id", l.ID, "budget", budgetID)
	return &l, nil
}

// ListLimits returns all limits of one budget (org-scoped via the budget).
func (h *Handler) ListLimits(ctx context.Context, budgetID uuid.UUID) ([]BudgetLimit, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, `SELECT bl.id, bl.budget_id, bl.category_id, bl.limit_minor, bl.currency, bl.include_subcategories, COALESCE(bl.alert_pct, 80) FROM budget.budget_limits bl JOIN budget.budgets b ON b.id = bl.budget_id WHERE bl.budget_id = $1 AND b.org_id = $2 ORDER BY bl.id`, budgetID, orgID)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()
	var out []BudgetLimit
	for rows.Next() {
		var l BudgetLimit
		if err := rows.Scan(&l.ID, &l.BudgetID, &l.CategoryID, &l.LimitMinor, &l.Currency, &l.IncludeSubcategories, &l.AlertPct); err != nil {
			return nil, common.Wrap(err)
		}
		out = append(out, l)
	}
	return out, common.Wrap(rows.Err())
}

// UpdateLimit changes limit_minor/alert_pct/include_subcategories. Nil pointers
// leave the field untouched.
func (h *Handler) UpdateLimit(ctx context.Context, id uuid.UUID, limitMinor *int64, alertPct *int, includeSub *bool) (*BudgetLimit, error) {
	var l BudgetLimit
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		orgID, err := common.ActingOrg(ctx, h.pool)
		if err != nil {
			return err
		}
		var cur BudgetLimit
		err = tx.QueryRow(ctx, `SELECT bl.id, bl.budget_id, bl.category_id, bl.limit_minor, bl.currency, bl.include_subcategories, COALESCE(bl.alert_pct, 80) FROM budget.budget_limits bl JOIN budget.budgets b ON b.id = bl.budget_id WHERE bl.id = $1 AND b.org_id = $2 FOR UPDATE`, id, orgID).
			Scan(&cur.ID, &cur.BudgetID, &cur.CategoryID, &cur.LimitMinor, &cur.Currency, &cur.IncludeSubcategories, &cur.AlertPct)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("budget limit not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if limitMinor != nil {
			cur.LimitMinor = *limitMinor
		}
		if alertPct != nil {
			cur.AlertPct = *alertPct
		}
		if includeSub != nil {
			cur.IncludeSubcategories = *includeSub
		}
		if err := validateLimit(cur.LimitMinor, cur.AlertPct); err != nil {
			return common.Wrap(err)
		}
		err = tx.QueryRow(ctx, `UPDATE budget.budget_limits SET limit_minor = $2, alert_pct = $3, include_subcategories = $4 WHERE id = $1 RETURNING id, budget_id, category_id, limit_minor, currency, include_subcategories, alert_pct`,
			cur.ID, cur.LimitMinor, cur.AlertPct, cur.IncludeSubcategories).
			Scan(&l.ID, &l.BudgetID, &l.CategoryID, &l.LimitMinor, &l.Currency, &l.IncludeSubcategories, &l.AlertPct)
		return common.Wrap(err)
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("budget limit updated", "id", l.ID)
	return &l, nil
}

// DeleteLimit removes one limit; its budget_progress row cascades (§schema).
func (h *Handler) DeleteLimit(ctx context.Context, id uuid.UUID) error {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return err
	}
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM budget.budget_limits bl USING budget.budgets b WHERE bl.id = $1 AND bl.budget_id = b.id AND b.org_id = $2`, id, orgID)
		if err != nil {
			return common.Wrap(err)
		}
		if tag.RowsAffected() == 0 {
			return common.Wrap(common.NotFound("budget limit not found"))
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.log.Info("budget limit deleted", "id", id)
	return nil
}

// GetProgress returns the current progress row for one limit, or NotFound when
// no transaction event has materialized it yet.
func (h *Handler) GetProgress(ctx context.Context, limitID uuid.UUID) (*BudgetProgress, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	var p BudgetProgress
	err = h.pool.QueryRow(ctx, `SELECT p.budget_limit_id, p.spent_minor, p.period_start, p.period_end, p.updated_at FROM budget.budget_progress p JOIN budget.budget_limits bl ON bl.id = p.budget_limit_id JOIN budget.budgets b ON b.id = bl.budget_id WHERE p.budget_limit_id = $1 AND b.org_id = $2`, limitID, orgID).
		Scan(&p.BudgetLimitID, &p.SpentMinor, &p.PeriodStart, &p.PeriodEnd, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, common.Wrap(common.NotFound("budget progress not found"))
	}
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &p, nil
}

// CreateBudgetPlan inserts a new budget plan for the acting org.
func (h *Handler) CreateBudgetPlan(ctx context.Context, periodType string, periodStart time.Time, refType string, categoryID uuid.UUID, accountID uuid.UUID, amountMinor int64, currency string) (*BudgetPlan, error) {
	if periodType != "month" && periodType != "quarter" && periodType != "year" {
		return nil, common.BadRequest("period_type must be one of month, quarter, year")
	}
	if refType != "category" && refType != "account" {
		return nil, common.BadRequest("ref_type must be one of category, account")
	}
	if refType == "category" && categoryID == uuid.Nil {
		return nil, common.BadRequest("category_id is required when ref_type is category")
	}
	if refType == "account" && accountID == uuid.Nil {
		return nil, common.BadRequest("account_id is required when ref_type is account")
	}
	if amountMinor < 0 {
		return nil, common.BadRequest("amount_minor must be non-negative")
	}

	var bp BudgetPlan
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		orgID, err := common.ActingOrg(ctx, h.pool)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `INSERT INTO budget.budget_plans (org_id, period_type, period_start, ref_type, category_id, account_id, amount_minor, currency) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, org_id, period_type, period_start, ref_type, category_id, account_id, amount_minor, currency, created_at, updated_at`,
			orgID, periodType, periodStart, refType, categoryID, accountID, amountMinor, currency)
		return row.Scan(&bp.ID, &bp.OrgID, &bp.PeriodType, &bp.PeriodStart, &bp.RefType, &bp.CategoryID, &bp.AccountID, &bp.AmountMinor, &bp.Currency, &bp.CreatedAt, &bp.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("budget plan created", "id", bp.ID, "period_type", periodType)
	return &bp, nil
}

// ListBudgetPlans returns all budget plans of the acting org.
func (h *Handler) ListBudgetPlans(ctx context.Context) ([]BudgetPlan, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	rows, err := h.pool.Query(ctx, `SELECT id, org_id, period_type, period_start, ref_type, category_id, account_id, amount_minor, currency, created_at, updated_at FROM budget.budget_plans WHERE org_id = $1 ORDER BY created_at ASC`, orgID)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()
	var out []BudgetPlan
	for rows.Next() {
		var bp BudgetPlan
		if err := rows.Scan(&bp.ID, &bp.OrgID, &bp.PeriodType, &bp.PeriodStart, &bp.RefType, &bp.CategoryID, &bp.AccountID, &bp.AmountMinor, &bp.Currency, &bp.CreatedAt, &bp.UpdatedAt); err != nil {
			return nil, common.Wrap(err)
		}
		out = append(out, bp)
	}
	return out, common.Wrap(rows.Err())
}

// GetBudgetPlan returns one budget plan by id, or NotFound when it does not exist or
// belongs to another org.
func (h *Handler) GetBudgetPlan(ctx context.Context, id uuid.UUID) (*BudgetPlan, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	var bp BudgetPlan
	err = h.pool.QueryRow(ctx, `SELECT id, org_id, period_type, period_start, ref_type, category_id, account_id, amount_minor, currency, created_at, updated_at FROM budget.budget_plans WHERE id = $1 AND org_id = $2`, id, orgID).
		Scan(&bp.ID, &bp.OrgID, &bp.PeriodType, &bp.PeriodStart, &bp.RefType, &bp.CategoryID, &bp.AccountID, &bp.AmountMinor, &bp.Currency, &bp.CreatedAt, &bp.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, common.Wrap(common.NotFound("budget plan not found"))
	}
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &bp, nil
}

// UpdateBudgetPlan changes amount_minor/currency. Nil pointers leave the field
// untouched.
func (h *Handler) UpdateBudgetPlan(ctx context.Context, id uuid.UUID, amountMinor *int64, currency *string) (*BudgetPlan, error) {
	var bp BudgetPlan
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		orgID, err := common.ActingOrg(ctx, h.pool)
		if err != nil {
			return err
		}
		var cur BudgetPlan
		err = tx.QueryRow(ctx, `SELECT id, org_id, period_type, period_start, ref_type, category_id, account_id, amount_minor, currency, created_at, updated_at FROM budget.budget_plans WHERE id = $1 AND org_id = $2 FOR UPDATE`, id, orgID).
			Scan(&cur.ID, &cur.OrgID, &cur.PeriodType, &cur.PeriodStart, &cur.RefType, &cur.CategoryID, &cur.AccountID, &cur.AmountMinor, &cur.Currency, &cur.CreatedAt, &cur.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("budget plan not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if amountMinor != nil {
			cur.AmountMinor = *amountMinor
		}
		if currency != nil {
			cur.Currency = *currency
		}
		err = tx.QueryRow(ctx, `UPDATE budget.budget_plans SET amount_minor = $2, currency = $3 WHERE id = $1 RETURNING id, org_id, period_type, period_start, ref_type, category_id, account_id, amount_minor, currency, created_at, updated_at`,
			cur.ID, cur.AmountMinor, cur.Currency).
			Scan(&bp.ID, &bp.OrgID, &bp.PeriodType, &bp.PeriodStart, &bp.RefType, &bp.CategoryID, &bp.AccountID, &bp.AmountMinor, &bp.Currency, &bp.CreatedAt, &bp.UpdatedAt)
		return common.Wrap(err)
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("budget plan updated", "id", bp.ID)
	return &bp, nil
}

// DeleteBudgetPlan removes one budget plan.
func (h *Handler) DeleteBudgetPlan(ctx context.Context, id uuid.UUID) error {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return err
	}
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM budget.budget_plans WHERE id = $1 AND org_id = $2`, id, orgID)
		if err != nil {
			return common.Wrap(err)
		}
		if tag.RowsAffected() == 0 {
			return common.Wrap(common.NotFound("budget plan not found"))
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.log.Info("budget plan deleted", "id", id)
	return nil
}
