package transactions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/db"
)

// CreateTransferInput mirrors the GraphQL input (ARCHITECTURE §6.7). Currency is the
// source account's currency; the target-leg amount is converted to its currency.
type CreateTransferInput struct {
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	AmountMinor   common.Bigint
	Currency      string // source (from) currency
	Description   *string
	OccurredAt    time.Time
}

// TransferOutput is returned by createTransfer. FromBalanceMinor/ToBalanceMinor are
// the two accounts' balances after both legs are applied (§8.3 step 6).
type TransferOutput struct {
	TransactionID    uuid.UUID `json:"transaction_id"`
	FromBalanceMinor int64     `json:"from_balance_minor"`
	ToBalanceMinor   int64     `json:"to_balance_minor"`
	FXApplied        bool      `json:"fx_applied"`
}

// CreateTransfer performs a two-sided transfer in one SERIALIZABLE transaction with
// retry (§8.3). It locks both accounts in ascending id order to avoid deadlocks,
// converts the amount into the target currency when needed, inserts a single
// transfer row and writes two balance_ledger entries (source −amount, target +amount).
func (h *Handler) CreateTransfer(ctx context.Context, input CreateTransferInput) (*TransferOutput, error) {
	if err := validateKind("transfer"); err != nil {
		return nil, common.Wrap(err)
	}
	amount, err := input.AmountMinor.Int64()
	if err != nil || amount <= 0 {
		return nil, common.Wrap(common.BadRequest("amount_minor must be a positive integer"))
	}
	if !common.ValidCurrency(ctx, h.pool, input.Currency) {
		return nil, common.Wrap(common.BadRequest(fmt.Sprintf("unknown currency %q", input.Currency)))
	}

	curOrg, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}

	var out TransferOutput
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		// Lock both accounts in ascending id order (§8.0 deadlock protection). The
		// query returns them ordered by id, so the first row is always the source.
		type acct struct {
			ID       uuid.UUID
			OrgID    uuid.UUID
			Currency string
		}
		var from, to acct
		row := tx.QueryRow(ctx, `SELECT id, org_id, currency FROM account.accounts WHERE id = ANY($1) AND deleted_at IS NULL ORDER BY id ASC FOR UPDATE`, []uuid.UUID{input.FromAccountID, input.ToAccountID})
		if err := row.Scan(&from.ID, &from.OrgID, &from.Currency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return common.Wrap(common.NotFound("source account not found"))
			}
			return common.Wrap(err)
		}
		if err := row.Scan(&to.ID, &to.OrgID, &to.Currency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return common.Wrap(common.NotFound("target account not found"))
			}
			return common.Wrap(err)
		}

		// Both accounts must belong to the acting user's organization.
		if from.OrgID != curOrg || to.OrgID != curOrg {
			return common.Wrap(common.NotFound("account not found"))
		}

		// A transfer cannot move money within the same account (§schema invariant).
		if from.ID == to.ID {
			return common.Wrap(common.BadRequest("cannot transfer to the same account"))
		}

		// Convert into the target currency using the nearest historical rate.
		delta, fxApplied, err := h.convertToAccountMinor(ctx, amount, from.Currency, to.Currency, input.OccurredAt)
		if err != nil {
			return common.Wrap(err)
		}
		if delta < 0 {
			return common.Wrap(common.BadRequest("converted amount must be non-negative"))
		}

		var txID uuid.UUID
		err = tx.QueryRow(ctx, `INSERT INTO tx.transactions (org_id, account_id, to_account_id, kind, amount_minor, currency, amount_in_account_currency_minor, description, occurred_at, source) VALUES ($1, $2, $3, 'transfer', $4, $5, $6, $7, $8, 'manual') RETURNING id`,
			curOrg, from.ID, to.ID, amount, from.Currency, delta, input.Description, input.OccurredAt).Scan(&txID)
		if err != nil {
			return common.Wrap(err)
		}

		// Two ledger rows: source −amount, target +amount (§8.3 step 5). Each call
		// also adjusts the corresponding balance.
		if err := applyLedgerAndBalance(ctx, tx, from.ID, -delta, "transfer", txID); err != nil {
			return common.Wrap(err)
		}
		if err := applyLedgerAndBalance(ctx, tx, to.ID, delta, "transfer", txID); err != nil {
			return common.Wrap(err)
		}

		var fb, tb int64
		err = tx.QueryRow(ctx, `SELECT balance_minor FROM account.accounts WHERE id = $1`, from.ID).Scan(&fb)
		if err != nil {
			return common.Wrap(err)
		}
		err = tx.QueryRow(ctx, `SELECT balance_minor FROM account.accounts WHERE id = $1`, to.ID).Scan(&tb)
		if err != nil {
			return common.Wrap(err)
		}
		out.TransactionID = txID
		out.FromBalanceMinor = fb
		out.ToBalanceMinor = tb
		out.FXApplied = fxApplied
		h.log.Info("transfer created", "id", out.TransactionID, "from", from.ID, "to", to.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
