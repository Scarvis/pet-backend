package transactions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/config"
	"github.com/Scarvis/pet-backend/internal/db"
)

// Handler serves the transaction Actions (create/update/delete/transfer). It owns
// the db pool and logger; cmd/server wires it to HTTP endpoints (§7.1).
type Handler struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewHandler builds a transactions Handler.
func NewHandler(pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{pool: pool, log: log}
}

// CreateTransactionInput mirrors the GraphQL input (ARCHITECTURE §6.7). CategoryID
// is optional; Description and OccurredAt are timestamps.
type CreateTransactionInput struct {
	AccountID   uuid.UUID
	CategoryID  *uuid.UUID
	Kind        string // income | expense
	AmountMinor common.Bigint
	Currency    string
	Description *string
	OccurredAt  time.Time
}

// CreateTransactionOutput is returned to the client (§6.7). NewBalanceMinor is the
// account balance after the insert; it is an int64 here and rendered as a Bigint at
// the GraphQL boundary (Phase 8).
type CreateTransactionOutput struct {
	TransactionID   uuid.UUID `json:"transaction_id"`
	NewBalanceMinor int64     `json:"new_balance_minor"`
	FXApplied       bool      `json:"fx_applied"`
}

// UpdateTransactionInput is a partial update: only the balance-affecting fields are
// meaningful (ARCHITECTURE §6.7). Category changes do not affect balances and are
// handled by Hasura directly (§6.3), so they are not part of this Action.
type UpdateTransactionInput struct {
	TransactionID uuid.UUID
	AccountID     *uuid.UUID
	Kind          *string
	AmountMinor   *common.Bigint
	Currency      *string
	OccurredAt    *time.Time
}

// DeleteTransactionInput only carries the id (§6.7).
type DeleteTransactionInput struct {
	TransactionID uuid.UUID
}

// TransactionMutationOutput is returned by create and delete (§6.7). For a transfer,
// NewBalanceMinor holds the balance of the lower-id (source) account after reversal.
type TransactionMutationOutput struct {
	TransactionID   uuid.UUID `json:"transaction_id"`
	NewBalanceMinor int64     `json:"new_balance_minor"`
}

// CreateTransaction validates its input and, in one SERIALIZABLE transaction with
// retry (§8.0), inserts the row, writes a balance_ledger entry and adjusts the
// account balance (with FX conversion when currencies differ). The trigger publishes
// the event to app.events by itself (§8.2 step 7).
func (h *Handler) CreateTransaction(ctx context.Context, input CreateTransactionInput) (*CreateTransactionOutput, error) {
	if err := validateKind(input.Kind); err != nil {
		return nil, common.Wrap(err)
	}
	amount, err := input.AmountMinor.Int64()
	if err != nil || amount <= 0 {
		return nil, common.Wrap(common.BadRequest("amount_minor must be a positive integer"))
	}
	if !common.ValidCurrency(ctx, h.pool, input.Currency) {
		return nil, common.BadRequest(fmt.Sprintf("unknown currency %q", input.Currency))
	}

	var out CreateTransactionOutput
	curOrg, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		var accID, accOrg uuid.UUID
		var currency string
		if err := tx.QueryRow(ctx, `SELECT id, org_id, currency FROM account.accounts WHERE id = $1 AND deleted_at IS NULL`, input.AccountID).Scan(&accID, &accOrg, &currency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return common.Wrap(common.NotFound("account not found"))
			}
			return common.Wrap(err)
		}
		if accOrg != curOrg {
			return common.Wrap(common.NotFound("account not found"))
		}
		if input.CategoryID != nil {
			var catOrg uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT org_id FROM tx.categories WHERE id = $1`, *input.CategoryID).Scan(&catOrg); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return common.Wrap(common.NotFound("category not found"))
				}
				return common.Wrap(err)
			}
			if catOrg != curOrg {
				return common.Wrap(common.NotFound("category not found"))
			}
		}

		delta, fxApplied, err := h.convertToAccountMinor(ctx, amount, input.Currency, currency, input.OccurredAt)
		if err != nil {
			return common.Wrap(err)
		}
		if delta < 0 {
			return common.Wrap(common.BadRequest("converted amount must be non-negative"))
		}

		var txID uuid.UUID
		err = tx.QueryRow(ctx, `INSERT INTO tx.transactions (org_id, account_id, category_id, kind, amount_minor, currency, amount_in_account_currency_minor, description, occurred_at, source) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'manual') RETURNING id`,
			curOrg, input.AccountID, input.CategoryID, input.Kind, amount, input.Currency, delta, input.Description, input.OccurredAt).Scan(&txID)
		if err != nil {
			return common.Wrap(err)
		}

		sign := int64(1)
		if input.Kind == "expense" {
			sign = -1
		}
		if err := applyLedgerAndBalance(ctx, tx, accID, sign*delta, "transaction", txID); err != nil {
			return common.Wrap(err)
		}

		var newBalance int64
		err = tx.QueryRow(ctx, `SELECT balance_minor FROM account.accounts WHERE id = $1`, accID).Scan(&newBalance)
		if err != nil {
			return common.Wrap(err)
		}
		out.TransactionID = txID
		out.NewBalanceMinor = newBalance
		out.FXApplied = fxApplied
		return nil
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("transaction created", "id", out.TransactionID, "kind", input.Kind)
	return &out, nil
}

// UpdateTransaction reverses the original effect and applies the new one in a single
// SERIALIZABLE transaction (§8.2 update). Only balance-affecting fields are supported;
// if none changed it is a no-op. Transfers cannot be updated through this Action.
func (h *Handler) UpdateTransaction(ctx context.Context, input UpdateTransactionInput) (*CreateTransactionOutput, error) {
	curOrg, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}

	var out CreateTransactionOutput
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL app.via_action = 'on'"); err != nil {
			return common.Wrap(err)
		}

		var txID uuid.UUID
		var kind string
		var accountID *uuid.UUID
		var currency string
		var amountMinor int64
		var occurredAt time.Time
		err := tx.QueryRow(ctx, `SELECT id, kind, account_id, currency, amount_minor, occurred_at FROM tx.transactions WHERE id = $1 FOR UPDATE`, input.TransactionID).Scan(&txID, &kind, &accountID, &currency, &amountMinor, &occurredAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("transaction not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if kind == "transfer" {
			return common.Wrap(common.BadRequest("transfers cannot be updated; delete and recreate instead"))
		}

		var orgCol uuid.UUID
		err = tx.QueryRow(ctx, `SELECT org_id FROM tx.transactions WHERE id = $1`, input.TransactionID).Scan(&orgCol)
		if err != nil {
			return common.Wrap(err)
		}
		if orgCol != curOrg {
			return common.Wrap(common.NotFound("transaction not found"))
		}

		// Resolve the new values, falling back to the current ones for unset fields.
		newAccount := accountID
		if input.AccountID != nil {
			newAccount = input.AccountID
		}
		newKind := kind
		if input.Kind != nil {
			newKind = *input.Kind
		}
		newAmount := amountMinor
		if input.AmountMinor != nil {
			v, perr := input.AmountMinor.Int64()
			if perr != nil || v <= 0 {
				return common.Wrap(common.BadRequest("amount_minor must be a positive integer"))
			}
			newAmount = v
		}
		newCurrency := currency
		if input.Currency != nil {
			newCurrency = *input.Currency
		}
		newOccurredAt := occurredAt
		if input.OccurredAt != nil {
			newOccurredAt = *input.OccurredAt
		}

		changed := *newAccount != *accountID || newKind != kind || newAmount != amountMinor || newCurrency != currency || !sameDay(newOccurredAt, occurredAt)
		if !changed {
			return nil // nothing balance-relevant changed; balances are left as-is
		}

		// Undo the original effect on every account it touched (§8.2 step 4).
		if _, err := reverseEffect(ctx, tx, txID); err != nil {
			return common.Wrap(err)
		}

		// Determine the target account id and its currency for the new effect. When a
		// different account is supplied we look up its currency; otherwise we keep the
		// original transaction's currency (already loaded above).
		target := *newAccount
		var targetCurrency string
		if input.AccountID != nil {
			var targetOrg uuid.UUID
			err = tx.QueryRow(ctx, `SELECT org_id, currency FROM account.accounts WHERE id = $1 AND deleted_at IS NULL`, *input.AccountID).Scan(&targetOrg, &targetCurrency)
			if errors.Is(err, pgx.ErrNoRows) {
				return common.Wrap(common.NotFound("account not found"))
			}
			if err != nil {
				return common.Wrap(err)
			}
			if targetOrg != curOrg {
				return common.Wrap(common.NotFound("account not found"))
			}
		} else {
			targetCurrency = currency // existing account's currency (loaded above)
		}

		delta, fxApplied, err := h.convertToAccountMinor(ctx, newAmount, newCurrency, targetCurrency, newOccurredAt)
		if err != nil {
			return common.Wrap(err)
		}
		sign := int64(1)
		if newKind == "expense" {
			sign = -1
		}
		if delta < 0 {
			return common.Wrap(common.BadRequest("converted amount must be non-negative"))
		}
		if err := applyLedgerAndBalance(ctx, tx, target, sign*delta, "transaction", txID); err != nil {
			return common.Wrap(err)
		}

		_, err = tx.Exec(ctx, `UPDATE tx.transactions SET amount_minor = $2, currency = $3, kind = $4, occurred_at = $5 WHERE id = $1 AND deleted_at IS NULL`, txID, newAmount, newCurrency, newKind, newOccurredAt)
		if err != nil {
			return common.Wrap(err)
		}

		var nb int64
		err = tx.QueryRow(ctx, `SELECT balance_minor FROM account.accounts WHERE id = $1`, target).Scan(&nb)
		if err != nil {
			return common.Wrap(err)
		}
		out.TransactionID = txID
		out.NewBalanceMinor = nb
		out.FXApplied = fxApplied
		return nil
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("transaction updated", "id", out.TransactionID)
	return &out, nil
}

// DeleteTransaction soft-deletes a transaction and reverses its balance effect in one
// SERIALIZABLE transaction (§8.2 delete). It locks the affected accounts in ascending
// id order to avoid deadlocks, computes the reversal from the ledger (not today's FX),
// writes adjustment rows and marks the row deleted. The trigger then publishes
// `transaction.deleted`.
func (h *Handler) DeleteTransaction(ctx context.Context, input DeleteTransactionInput) (*TransactionMutationOutput, error) {
	curOrg, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}

	var out TransactionMutationOutput
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL app.via_action = 'on'"); err != nil {
			return common.Wrap(err)
		}

		var txID uuid.UUID
		var kind string
		var accountID *uuid.UUID
		var toAccountID *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id, kind, account_id, to_account_id FROM tx.transactions WHERE id = $1 FOR UPDATE`, input.TransactionID).Scan(&txID, &kind, &accountID, &toAccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("transaction not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}

		var orgCol uuid.UUID
		err = tx.QueryRow(ctx, `SELECT org_id FROM tx.transactions WHERE id = $1`, input.TransactionID).Scan(&orgCol)
		if err != nil {
			return common.Wrap(err)
		}
		if orgCol != curOrg {
			return common.Wrap(common.NotFound("transaction not found"))
		}

		// Lock every affected account in ascending id order (§8.0 deadlock protection).
		var ids []uuid.UUID
		switch kind {
		case "transfer":
			if toAccountID == nil || accountID == nil {
				return common.Wrap(common.BadRequest("transfer requires a target account"))
			}
			ids = []uuid.UUID{*accountID, *toAccountID}
		default:
			if accountID == nil {
				return common.Wrap(common.BadRequest("transaction has no account"))
			}
			ids = []uuid.UUID{*accountID}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
		for _, id := range ids {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM account.accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id); err != nil {
				return common.Wrap(err)
			}
		}

		// Reverse the original effect from the ledger (§8.2 step 4).
		if _, err := reverseEffect(ctx, tx, txID); err != nil {
			return common.Wrap(err)
		}

		// Mark the transaction deleted (§8.2 step 7).
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE tx.transactions SET deleted_at = $1 WHERE id = $2 AND org_id = $3`, now, txID, curOrg); err != nil {
			return common.Wrap(err)
		}

		// Report the balance of the primary (lower-id) affected account.
		var primary uuid.UUID
		if accountID != nil && toAccountID == nil {
			primary = *accountID
		} else if len(ids) > 0 {
			primary = ids[0]
		}
		var nb int64
		err = tx.QueryRow(ctx, `SELECT balance_minor FROM account.accounts WHERE id = $1 AND deleted_at IS NULL`, primary).Scan(&nb)
		if err != nil {
			return common.Wrap(err)
		}
		out.TransactionID = txID
		out.NewBalanceMinor = nb
		return nil
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("transaction deleted", "id", out.TransactionID)
	return &out, nil
}

// validateKind rejects anything outside the allowed set with a 400.
func validateKind(kind string) error {
	switch kind {
	case "income", "expense", "transfer":
		return nil
	default:
		return common.BadRequest("kind must be one of income, expense, transfer")
	}
}

// sameDay reports whether two timestamps fall on the same calendar day. Rates are
// stored per day (§8.2), so a change only matters when the day changes.
func sameDay(a, b time.Time) bool {
	return a.Truncate(24 * time.Hour).Equal(b.Truncate(24 * time.Hour))
}
