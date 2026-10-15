// Route handlers for the Hasura Actions (§6.4), Events (§6.5) and Cron
// webhooks (§6.6). Each handler decodes the Hasura envelope, injects the
// acting user, calls its domain handler (which owns the SERIALIZABLE
// transaction, §8.0), and renders JSON. Amounts cross the wire as Bigint
// decimal strings (§6.7); side effects stay post-COMMIT (§8.0/§8.8).
package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Scarvis/pet-backend/internal/analytics"
	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/imports"
	"github.com/Scarvis/pet-backend/internal/invest"
	"github.com/Scarvis/pet-backend/internal/observability"
	"github.com/Scarvis/pet-backend/internal/transactions"
)

// registerActionRoutes mounts the synchronous business-logic Actions (§6.4).
func registerActionRoutes(r chi.Router, s *server) {
	post(r, "register", "/actions/auth/register", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in authCredentials) (tokenResponse, error) {
			return issueTokens(ctx, s, in.Email, in.Password, true)
		})
	})
	post(r, "login", "/actions/auth/login", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in authCredentials) (tokenResponse, error) {
			return issueTokens(ctx, s, in.Email, in.Password, false)
		})
	})
	post(r, "refreshToken", "/actions/auth/refresh", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in refreshRequest) (tokenResponse, error) {
			out, err := s.auth.RefreshToken(ctx, in.RefreshToken)
			if err != nil {
				return tokenResponse{}, err
			}
			return toTokenResponse(out), nil
		})
	})
	post(r, "createAccount", "/actions/account/create", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in createAccountRequest) (*accountResponse, error) {
			acc, err := s.accounts.Create(ctx, in.Name, in.Type, in.Currency, in.IsHidden)
			if err != nil {
				return nil, err
			}
			return toAccountResponse(acc), nil
		})
	})
	post(r, "createTransaction", "/actions/tx/create", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in createTransactionRequest) (*transactionResult, error) {
			out, err := s.tx.CreateTransaction(ctx, transactions.CreateTransactionInput{
				AccountID:   in.AccountID,
				CategoryID:  in.CategoryID,
				Kind:        in.Kind,
				AmountMinor: in.AmountMinor,
				Currency:    in.Currency,
				Description: in.Description,
				OccurredAt:  in.OccurredAt,
			})
			if err != nil {
				return nil, err
			}
			return &transactionResult{
				TransactionID:   out.TransactionID,
				NewBalanceMinor: toBigint(out.NewBalanceMinor),
				FXApplied:       out.FXApplied,
			}, nil
		})
	})
	post(r, "createTransfer", "/actions/tx/transfer", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in createTransferRequest) (*transferResult, error) {
			out, err := s.tx.CreateTransfer(ctx, transactions.CreateTransferInput{
				FromAccountID: in.FromAccountID,
				ToAccountID:   in.ToAccountID,
				AmountMinor:   in.AmountMinor,
				Currency:      in.Currency,
				Description:   in.Description,
				OccurredAt:    in.OccurredAt,
			})
			if err != nil {
				return nil, err
			}
			return &transferResult{
				TransactionID:    out.TransactionID,
				FromBalanceMinor: toBigint(out.FromBalanceMinor),
				ToBalanceMinor:   toBigint(out.ToBalanceMinor),
				FXApplied:        out.FXApplied,
			}, nil
		})
	})
	post(r, "updateTransaction", "/actions/tx/update", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in updateTransactionRequest) (*transactionResult, error) {
			out, err := s.tx.UpdateTransaction(ctx, transactions.UpdateTransactionInput{
				TransactionID: in.TransactionID,
				AccountID:     in.AccountID,
				Kind:          in.Kind,
				AmountMinor:   in.AmountMinor,
				Currency:      in.Currency,
				OccurredAt:    in.OccurredAt,
			})
			if err != nil {
				return nil, err
			}
			return &transactionResult{
				TransactionID:   out.TransactionID,
				NewBalanceMinor: toBigint(out.NewBalanceMinor),
				FXApplied:       out.FXApplied,
			}, nil
		})
	})
	post(r, "deleteTransaction", "/actions/tx/delete", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in deleteTransactionRequest) (*deleteTransactionResult, error) {
			out, err := s.tx.DeleteTransaction(ctx, transactions.DeleteTransactionInput{
				TransactionID: in.TransactionID,
			})
			if err != nil {
				return nil, err
			}
			return &deleteTransactionResult{
				TransactionID:   out.TransactionID,
				NewBalanceMinor: toBigint(out.NewBalanceMinor),
			}, nil
		})
	})
	post(r, "createTrade", "/actions/invest/trade", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in createTradeRequest) (*tradeResult, error) {
			qty, err := parseNumeric(in.Quantity)
			if err != nil {
				return nil, err
			}
			out, err := s.invest.ExecuteTrade(ctx, invest.CreateTradeInput{
				PortfolioID: in.PortfolioID,
				AssetID:     in.AssetID,
				Side:        in.Side,
				Quantity:    qty,
				PriceMinor:  in.PriceMinor,
				ExecutedAt:  in.ExecutedAt,
			})
			if err != nil {
				return nil, err
			}
			return &tradeResult{
				HoldingID:    out.HoldingID,
				Quantity:     out.Quantity,
				AvgCostMinor: toBigint(out.AvgCostMinor),
			}, nil
		})
	})
	post(r, "startImport", "/actions/import/start", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in startImportRequest) (*startImportResult, error) {
			content, err := base64.StdEncoding.DecodeString(in.FileBase64)
			if err != nil {
				if content, err = base64.URLEncoding.DecodeString(in.FileBase64); err != nil {
					return nil, common.BadRequest("file_base64 is not valid base64")
				}
			}
			out, err := s.imports.StartImport(ctx, imports.StartImportInput{
				AccountID:  in.AccountID,
				FileName:   in.FileName,
				FileFormat: in.FileFormat,
				Content:    content,
			})
			if err != nil {
				return nil, err
			}
			observeImportOutcome(ctx, s, out.JobID)
			return &startImportResult{JobID: out.JobID, TotalRows: out.TotalRows}, nil
		})
	})
	post(r, "fetchFxRates", "/actions/fx/fetch", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in fxFetchRequest) (*fxFetchResult, error) {
			return runFXFetch(ctx, s, in.Bases, in.AsOf)
		})
	})
	post(r, "rebuildAnalytics", "/actions/analytics/rebuild", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in analyticsRebuildRequest) (*okResult, error) {
			from := in.From
			if from == nil {
				d := time.Now().UTC().AddDate(0, 0, -30)
				from = &d
			}
			if err := s.analytics.Rebuild(ctx, *from); err != nil {
				return nil, err
			}
			return &okResult{OK: true}, nil
		})
	})
}

// registerEventRoutes mounts the asynchronous Hasura Event webhooks (§6.5).
// NOTIFY is only a wake-up signal (§8.8): every webhook drains app.events
// from the cursor, so the handlers below simply catch up.
func registerEventRoutes(r chi.Router, s *server) {
	event := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if err := s.dispatch.CatchUp(withIdentity(req.Context(), req, nil)); err != nil {
				common.WriteError(w, err)
				return
			}
			common.WriteJSON(w, &okResult{OK: true, Event: name})
		}
	}
	post(r, "transaction_inserted", "/events/tx/created", event("transaction_inserted"))
	post(r, "transaction_updated", "/events/tx/updated", event("transaction_updated"))
	post(r, "trade_inserted", "/events/invest/trade", event("trade_inserted"))
	post(r, "import_job_updated", "/events/import/job", event("import_job_updated"))
	post(r, "fx_rate_inserted", "/events/fx/rate", event("fx_rate_inserted"))
}

// registerCronRoutes mounts the scheduled-task webhooks (§6.6).
func registerCronRoutes(r chi.Router, s *server) {
	post(r, "fetch_fx_rates", "/cron/fetch_fx_rates", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in fxFetchRequest) (*fxFetchResult, error) {
			return runFXFetch(ctx, s, in.Bases, in.AsOf)
		})
	})
	post(r, "rebuild_daily_summary", "/cron/rebuild_daily_summary", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in analyticsRebuildRequest) (*okResult, error) {
			from := in.From
			if from == nil {
				// Daily scope: yesterday UTC; the rebuild is delete-then-insert
				// over [from, ∞), so recent days are always refreshed (§8.7).
				d := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
				from = &d
			}
			if err := analytics.RebuildDailySummary(ctx, s.pool, *from); err != nil {
				return nil, err
			}
			return &okResult{OK: true}, nil
		})
	})
	post(r, "snapshot_net_worth", "/cron/snapshot_net_worth", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, in snapshotRequest) (*okResult, error) {
			asOf := time.Now().UTC()
			if in.AsOf != nil {
				asOf = *in.AsOf
			}
			if err := analytics.SnapshotAllOrgs(ctx, s.pool, asOf); err != nil {
				return nil, err
			}
			return &okResult{OK: true}, nil
		})
	})
	post(r, "rotate_budget_progress", "/cron/rotate_budget_progress", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, _ emptyRequest) (*deletedResult, error) {
			// Progress rows are recalculated from scratch on transaction
			// events (§8.4); rotation drops rows whose period already ended
			// so the next event materializes the fresh period (weekly
			// periods start Monday, matching the cron schedule §6.6).
			tag, err := s.pool.Exec(ctx, `DELETE FROM budget.budget_progress WHERE period_end <= now()`)
			if err != nil {
				return nil, common.Wrap(err)
			}
			return &deletedResult{Deleted: tag.RowsAffected()}, nil
		})
	})
	post(r, "cleanup_tokens", "/cron/cleanup_tokens", func(w http.ResponseWriter, req *http.Request) {
		handle(w, req, func(ctx context.Context, _ emptyRequest) (*deletedResult, error) {
			tag, err := s.pool.Exec(ctx, `DELETE FROM identity.refresh_tokens WHERE expires_at < now()`)
			if err != nil {
				return nil, common.Wrap(err)
			}
			return &deletedResult{Deleted: tag.RowsAffected()}, nil
		})
	})
}

// runFXFetch loads daily rates for bases (defaulting to every org's base
// currency) as of asOf (default today UTC), timing the fetch for
// financeapp_fx_fetch_duration_seconds (§8.7 steps 1–2, §10).
func runFXFetch(ctx context.Context, s *server, bases []string, asOf *time.Time) (*fxFetchResult, error) {
	if len(bases) == 0 {
		var err error
		bases, err = orgBaseCurrencies(ctx, s)
		if err != nil {
			return nil, err
		}
	}
	day := time.Now().UTC()
	if asOf != nil {
		day = *asOf
	}
	start := time.Now()
	n, err := s.fx.FetchDailyRates(ctx, bases, day)
	if err != nil {
		return nil, err
	}
	observability.ObserveFXFetch(time.Since(start))
	return &fxFetchResult{RowsWritten: n}, nil
}

// orgBaseCurrencies lists every org's base currency for the FX cron default.
func orgBaseCurrencies(ctx context.Context, s *server) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT base_currency FROM identity.organizations`)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer rows.Close()
	var bases []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, common.Wrap(err)
		}
		if strings.TrimSpace(b) != "" {
			bases = append(bases, b)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, common.Wrap(err)
	}
	if len(bases) == 0 {
		return nil, common.BadRequest("no base currencies configured")
	}
	return bases, nil
}

// observeImportOutcome feeds financeapp_import_rows_total{job_id,status} from
// the finished job's raw_rows counts (§10). Read-only: it never fails the
// Action response.
func observeImportOutcome(ctx context.Context, s *server, jobID uuid.UUID) {
	rows, err := s.pool.Query(ctx, `SELECT status, COUNT(*) FROM import.raw_rows WHERE import_job_id = $1 GROUP BY status`, jobID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return
		}
		observability.ImportRowsTotal.WithLabelValues(jobID.String(), status).Add(float64(n))
	}
}
