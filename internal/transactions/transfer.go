package transactions

import (
	"context"
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
		// Lock both accounts in ascending id order (§8.0 deadlock protection), then
		// identify source and target by id: row order does not say which is which.
		from, to, err := lockTransferAccounts(ctx, tx, input.FromAccountID, input.ToAccountID)
		if err != nil {
			return err
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
		if err := applyLedgerAndBalance(ctx, tx, from.ID, -amount, "transfer", txID); err != nil {
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

type transferAccount struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	Currency string
}

// accountRows is the subset of pgx.Rows the lookup needs, so the scan logic can be
// unit-tested without a database.
type accountRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// lockTransferAccounts locks both accounts with one query, in ascending id order to
// avoid deadlocks, and returns them mapped to their roles by id.
func lockTransferAccounts(ctx context.Context, tx pgx.Tx, fromID, toID uuid.UUID) (transferAccount, transferAccount, error) {
	rows, err := tx.Query(ctx, `SELECT id, org_id, currency FROM account.accounts WHERE id = ANY($1) AND deleted_at IS NULL ORDER BY id ASC FOR UPDATE`, []uuid.UUID{fromID, toID})
	if err != nil {
		return transferAccount{}, transferAccount{}, common.Wrap(err)
	}
	defer rows.Close()

	return scanTransferAccounts(rows, fromID, toID)
}

func scanTransferAccounts(rows accountRows, fromID, toID uuid.UUID) (from, to transferAccount, err error) {
	var foundFrom, foundTo bool
	for rows.Next() {
		var account transferAccount
		if err := rows.Scan(&account.ID, &account.OrgID, &account.Currency); err != nil {
			return from, to, common.Wrap(err)
		}
		if account.ID == fromID {
			from, foundFrom = account, true
		}
		if account.ID == toID {
			to, foundTo = account, true
		}
	}
	if err := rows.Err(); err != nil {
		return from, to, common.Wrap(err)
	}
	if !foundFrom {
		return from, to, common.Wrap(common.NotFound("source account not found"))
	}
	if !foundTo {
		return from, to, common.Wrap(common.NotFound("target account not found"))
	}
	return from, to, nil
}
