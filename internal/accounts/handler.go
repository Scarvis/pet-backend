// Package accounts implements account CRUD and archiving (soft delete) for the
// core transactions domain (§7.1). Every mutating method runs inside a
// SERIALIZABLE transaction with retry so balances stay consistent under conflict.
package accounts

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

// accountTypes are the CHECK-constraint values for account.type (§3.2).
var accountTypes = map[string]bool{"cash": true, "card": true, "deposit": true, "debt": true}

// Handler serves the account Actions (create/list/get/archive/unarchive). It owns
// the db pool and logger; cmd/server wires it to HTTP endpoints (§7.1).
type Handler struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewHandler builds an accounts Handler.
func NewHandler(pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{pool: pool, log: log}
}

// Account is a bank/card/deposit account (§schema). DeletedAt is nil for active
// accounts; it holds the soft-delete timestamp once archived. IsHidden is a
// client-side visibility flag (hidden from default lists, still counted in
// balances and net worth); it is independent from archiving (deleted_at).
type Account struct {
	ID           uuid.UUID  `json:"id"`
	OrgID        uuid.UUID  `json:"org_id"`
	OwnerUserID  uuid.UUID  `json:"owner_user_id"`
	Name         string     `json:"name"`
	Type         string     `json:"type"` // cash | card | deposit | debt
	Currency     string     `json:"currency"`
	BalanceMinor int64      `json:"balance_minor"`
	IsArchived   bool       `json:"is_archived"`
	IsHidden     bool       `json:"is_hidden"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Create inserts a new account owned by the acting user within their organization.
func (h *Handler) Create(ctx context.Context, name, typ, currency string, isHidden bool) (*Account, error) {
	userID, err := common.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if !accountTypes[typ] {
		return nil, common.Wrap(common.BadRequest("account type must be one of cash, card, deposit, debt"))
	}
	if !common.ValidCurrency(ctx, h.pool, currency) {
		return nil, common.BadRequest(fmt.Sprintf("unknown currency %q", currency))
	}

	var acc Account
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		orgID, err := common.ActingOrg(ctx, h.pool)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `INSERT INTO account.accounts (org_id, owner_user_id, name, type, currency, is_hidden) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, org_id, owner_user_id, name, type, currency, balance_minor, is_archived, is_hidden, deleted_at, created_at, updated_at`,
			orgID, userID, name, typ, currency, isHidden)
		return row.Scan(&acc.ID, &acc.OrgID, &acc.OwnerUserID, &acc.Name, &acc.Type, &acc.Currency, &acc.BalanceMinor, &acc.IsArchived, &acc.IsHidden, &acc.DeletedAt, &acc.CreatedAt, &acc.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("account created", "id", acc.ID, "name", acc.Name)
	return &acc, nil
}

// List returns the active (non-deleted) accounts belonging to the acting user's org.
func (h *Handler) List(ctx context.Context) ([]Account, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}

	rows, err := h.pool.Query(ctx, `SELECT id, org_id, owner_user_id, name, type, currency, balance_minor, is_archived, is_hidden, deleted_at, created_at, updated_at FROM account.accounts WHERE org_id = $1 AND deleted_at IS NULL ORDER BY name ASC`, orgID)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var acc Account
		if err := scanAccount(ctx, rows, &acc); err != nil {
			return nil, common.Wrap(err)
		}
		out = append(out, acc)
	}
	return out, rows.Err()
}

// Get returns a single account by id. It is NotFound when the account does not exist
// or belongs to another organization (§7.1 ownership).
func (h *Handler) Get(ctx context.Context, id uuid.UUID) (*Account, error) {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}

	var acc Account
	err = h.pool.QueryRow(ctx, `SELECT id, org_id, owner_user_id, name, type, currency, balance_minor, is_archived, is_hidden, deleted_at, created_at, updated_at FROM account.accounts WHERE id = $1 AND org_id = $2`, id, orgID).Scan(
		&acc.ID, &acc.OrgID, &acc.OwnerUserID, &acc.Name, &acc.Type, &acc.Currency, &acc.BalanceMinor, &acc.IsArchived, &acc.IsHidden, &acc.DeletedAt, &acc.CreatedAt, &acc.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, common.Wrap(common.NotFound("account not found"))
	}
	if err != nil {
		return nil, common.Wrap(err)
	}
	return &acc, nil
}

// Archive soft-deletes an account (§soft delete). It is a no-op when the account is
// already archived and returns NotFound for unknown accounts.
func (h *Handler) Archive(ctx context.Context, id uuid.UUID) error {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return err
	}

	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		var deletedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT deleted_at FROM account.accounts WHERE id = $1 AND org_id = $2`, id, orgID).Scan(&deletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("account not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if deletedAt != nil {
			return nil // already archived; nothing to do
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE account.accounts SET deleted_at = $1 WHERE id = $2 AND org_id = $3`, now, id, orgID); err != nil {
			return common.Wrap(err)
		}
		h.log.Info("account archived", "id", id)
		return nil
	})
	return err
}

// Unarchive restores a previously archived account. It is a no-op when the account
// is already active and returns NotFound for unknown accounts.
func (h *Handler) Unarchive(ctx context.Context, id uuid.UUID) error {
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return err
	}

	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		var deletedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT deleted_at FROM account.accounts WHERE id = $1 AND org_id = $2`, id, orgID).Scan(&deletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("account not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if deletedAt == nil {
			return nil // already active; nothing to do
		}
		if _, err := tx.Exec(ctx, `UPDATE account.accounts SET deleted_at = NULL WHERE id = $1 AND org_id = $2`, id, orgID); err != nil {
			return common.Wrap(err)
		}
		h.log.Info("account restored", "id", id)
		return nil
	})
	return err
}

// scanAccount fills acc from a pgx rows source. It is shared by List and Get so the
// account shape stays consistent; NULL deleted_at scans into a nil *time.Time.
func scanAccount(ctx context.Context, src pgx.Rows, acc *Account) error {
	return src.Scan(&acc.ID, &acc.OrgID, &acc.OwnerUserID, &acc.Name, &acc.Type, &acc.Currency, &acc.BalanceMinor, &acc.IsArchived, &acc.IsHidden, &acc.DeletedAt, &acc.CreatedAt, &acc.UpdatedAt)
}
