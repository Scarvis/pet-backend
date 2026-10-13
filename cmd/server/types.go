// Request/response shapes for the Actions/Events/Cron HTTP boundary. Inputs
// mirror graphql/actions.graphql (§6.7); outputs render minor-unit amounts as
// Bigint decimal strings so they survive the GraphQL/JS wire (§6.7).
package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Scarvis/pet-backend/internal/accounts"
	"github.com/Scarvis/pet-backend/internal/auth"
	"github.com/Scarvis/pet-backend/internal/common"
)

// emptyRequest decodes `{}` for cron endpoints that take no arguments.
type emptyRequest struct{}

// authCredentials is the register/login input (§8.1).
type authCredentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// refreshRequest is the refreshToken input (§8.1).
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// tokenResponse carries an access + refresh pair; expires_in is seconds so
// the value is unambiguous on the wire.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func toTokenResponse(out *auth.IssueResult) tokenResponse {
	return tokenResponse{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		ExpiresIn:    int64(out.ExpiresIn / time.Second),
	}
}

// issueTokens runs register (new org) or login against the auth handler.
func issueTokens(ctx context.Context, s *server, email, password string, register bool) (tokenResponse, error) {
	var out *auth.IssueResult
	var err error
	if register {
		out, err = s.auth.Register(ctx, email, password)
	} else {
		out, err = s.auth.Login(ctx, email, password)
	}
	if err != nil {
		return tokenResponse{}, err
	}
	return toTokenResponse(out), nil
}

// createAccountRequest is the createAccount input (§6.4).
type createAccountRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Currency string `json:"currency"`
}

// accountResponse renders an account with its balance as a Bigint string.
type accountResponse struct {
	ID           uuid.UUID     `json:"id"`
	OrgID        uuid.UUID     `json:"org_id"`
	OwnerUserID  uuid.UUID     `json:"owner_user_id"`
	Name         string        `json:"name"`
	Type         string        `json:"type"`
	Currency     string        `json:"currency"`
	BalanceMinor common.Bigint `json:"balance_minor"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

func toAccountResponse(a *accounts.Account) *accountResponse {
	return &accountResponse{
		ID:           a.ID,
		OrgID:        a.OrgID,
		OwnerUserID:  a.OwnerUserID,
		Name:         a.Name,
		Type:         a.Type,
		Currency:     a.Currency,
		BalanceMinor: toBigint(a.BalanceMinor),
		CreatedAt:    a.CreatedAt,
		UpdatedAt:    a.UpdatedAt,
	}
}

// createTransactionRequest mirrors CreateTransactionInput (§6.7).
type createTransactionRequest struct {
	AccountID   uuid.UUID     `json:"account_id"`
	CategoryID  *uuid.UUID    `json:"category_id"`
	Kind        string        `json:"kind"`
	AmountMinor common.Bigint `json:"amount_minor"`
	Currency    string        `json:"currency"`
	Description *string       `json:"description"`
	OccurredAt  time.Time     `json:"occurred_at"`
}

// transactionResult mirrors CreateTransactionOutput (§6.7).
type transactionResult struct {
	TransactionID   uuid.UUID     `json:"transaction_id"`
	NewBalanceMinor common.Bigint `json:"new_balance_minor"`
	FXApplied       bool          `json:"fx_applied"`
}

// createTransferRequest mirrors CreateTransferInput (§6.7).
type createTransferRequest struct {
	FromAccountID uuid.UUID     `json:"from_account_id"`
	ToAccountID   uuid.UUID     `json:"to_account_id"`
	AmountMinor   common.Bigint `json:"amount_minor"`
	Currency      string        `json:"currency"`
	Description   *string       `json:"description"`
	OccurredAt    time.Time     `json:"occurred_at"`
}

// transferResult carries both legs' balances after a transfer (§8.3).
type transferResult struct {
	TransactionID    uuid.UUID     `json:"transaction_id"`
	FromBalanceMinor common.Bigint `json:"from_balance_minor"`
	ToBalanceMinor   common.Bigint `json:"to_balance_minor"`
	FXApplied        bool          `json:"fx_applied"`
}

// updateTransactionRequest mirrors UpdateTransactionInput (§6.7): only
// balance-affecting fields; nil leaves the field untouched.
type updateTransactionRequest struct {
	TransactionID uuid.UUID      `json:"transaction_id"`
	AccountID     *uuid.UUID     `json:"account_id"`
	Kind          *string        `json:"kind"`
	AmountMinor   *common.Bigint `json:"amount_minor"`
	Currency      *string        `json:"currency"`
	OccurredAt    *time.Time     `json:"occurred_at"`
}

// deleteTransactionRequest mirrors DeleteTransactionInput (§6.7).
type deleteTransactionRequest struct {
	TransactionID uuid.UUID `json:"transaction_id"`
}

// deleteTransactionResult mirrors TransactionMutationOutput (§6.7).
type deleteTransactionResult struct {
	TransactionID   uuid.UUID     `json:"transaction_id"`
	NewBalanceMinor common.Bigint `json:"new_balance_minor"`
}

// createTradeRequest mirrors CreateTradeInput (§6.7). Quantity is a GraphQL
// numeric: it may arrive as a JSON string or number (see parseNumeric).
type createTradeRequest struct {
	PortfolioID uuid.UUID       `json:"portfolio_id"`
	AssetID     uuid.UUID       `json:"asset_id"`
	Side        string          `json:"side"`
	Quantity    json.RawMessage `json:"quantity"`
	PriceMinor  common.Bigint   `json:"price_minor"`
	ExecutedAt  time.Time       `json:"executed_at"`
}

// tradeResult mirrors CreateTradeOutput (§6.7).
type tradeResult struct {
	HoldingID    uuid.UUID     `json:"holding_id"`
	Quantity     string        `json:"quantity"`
	AvgCostMinor common.Bigint `json:"avg_cost_minor"`
}

// startImportRequest mirrors StartImportInput (§6.7); the file travels as
// base64 and is decoded before StartImport.
type startImportRequest struct {
	AccountID  uuid.UUID `json:"account_id"`
	FileBase64 string    `json:"file_base64"`
	FileName   string    `json:"file_name"`
	FileFormat string    `json:"file_format"`
}

// startImportResult mirrors StartImportOutput (§6.7).
type startImportResult struct {
	JobID     uuid.UUID `json:"job_id"`
	TotalRows int       `json:"total_rows"`
}

// fxFetchRequest drives fetchFxRates (§6.4) and the fetch_fx_rates cron
// (§6.6): empty bases default to every org's base currency, nil as_of to
// today UTC.
type fxFetchRequest struct {
	Bases []string   `json:"bases"`
	AsOf  *time.Time `json:"as_of"`
}

// fxFetchResult reports the atomic daily load (§8.7 step 2).
type fxFetchResult struct {
	RowsWritten int `json:"rows_written"`
}

// analyticsRebuildRequest drives rebuildAnalytics (§6.4) and the
// rebuild_daily_summary cron (§6.6); nil From selects the endpoint default.
type analyticsRebuildRequest struct {
	From *time.Time `json:"from"`
}

// snapshotRequest drives the snapshot_net_worth cron (§6.6); nil AsOf means now.
type snapshotRequest struct {
	AsOf *time.Time `json:"as_of"`
}

// okResult acknowledges Events/Cron webhooks.
type okResult struct {
	OK    bool   `json:"ok"`
	Event string `json:"event,omitempty"`
}

// deletedResult reports cron cleanup counts.
type deletedResult struct {
	Deleted int64 `json:"deleted"`
}
