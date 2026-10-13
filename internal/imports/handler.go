// Import job orchestration (§8.6, §7.1): startImport Action plus the
// parse → map pipeline. The pipeline currently runs synchronously inside
// StartImport; Phase 8 (server wiring) can move mapping to a background worker
// without changing the stage functions. File persistence (local/S3 per
// STORAGE_BACKEND) is also Phase 8 — content arrives as bytes.
package imports

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

// fileFormats are the CHECK-constraint values for import_jobs.file_format.
var fileFormats = map[string]bool{"csv": true, "ofx": true}

// Handler serves the import Actions. It owns the db pool and logger;
// cmd/server wires it to HTTP endpoints (§7.1).
type Handler struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewHandler builds an imports Handler.
func NewHandler(pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{pool: pool, log: log}
}

// StartImportInput mirrors the GraphQL input (§6.7); content is the decoded
// file bytes (Phase 8 handles base64 + storage).
type StartImportInput struct {
	AccountID  uuid.UUID
	FileName   string
	FileFormat string // csv | ofx
	Content    []byte
}

// StartImportOutput is returned to the client (§6.7).
type StartImportOutput struct {
	JobID     uuid.UUID `json:"job_id"`
	TotalRows int       `json:"total_rows"`
}

// parseContent dispatches to the format parser (§8.6 step c).
func parseContent(format string, content []byte) ([]ParsedRow, error) {
	switch format {
	case "csv":
		return ParseCSV(content)
	case "ofx":
		return ParseOFX(content)
	default:
		return nil, common.BadRequest(fmt.Sprintf("unsupported file format %q", format))
	}
}

// StartImport creates the job (status='pending'), parses rows, inserts
// raw_rows with dedup hashes, then maps each row to a transaction (§8.6).
// Mapping errors on individual rows mark them 'error' without failing the
// job; only a stage-level failure marks the job 'failed'.
func (h *Handler) StartImport(ctx context.Context, input StartImportInput) (*StartImportOutput, error) {
	if !fileFormats[input.FileFormat] {
		return nil, common.Wrap(common.BadRequest("file_format must be one of csv, ofx"))
	}
	if input.FileName == "" {
		return nil, common.Wrap(common.BadRequest("file_name must not be empty"))
	}
	if len(input.Content) == 0 {
		return nil, common.Wrap(common.BadRequest("empty file"))
	}
	orgID, err := common.ActingOrg(ctx, h.pool)
	if err != nil {
		return nil, err
	}

	rows, err := parseContent(input.FileFormat, input.Content)
	if err != nil {
		return nil, common.Wrap(err)
	}

	var jobID uuid.UUID
	err = db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		var accOrg uuid.UUID
		err := tx.QueryRow(ctx, `SELECT org_id FROM account.accounts WHERE id = $1 AND deleted_at IS NULL`, input.AccountID).Scan(&accOrg)
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Wrap(common.NotFound("account not found"))
		}
		if err != nil {
			return common.Wrap(err)
		}
		if accOrg != orgID {
			return common.Wrap(common.NotFound("account not found"))
		}
		err = tx.QueryRow(ctx, `INSERT INTO import.import_jobs (org_id, account_id, file_name, file_format, status) VALUES ($1, $2, $3, $4, 'pending') RETURNING id`,
			orgID, input.AccountID, input.FileName, input.FileFormat).Scan(&jobID)
		return common.Wrap(err)
	})
	if err != nil {
		return nil, err
	}

	total, err := h.runPipeline(ctx, jobID, orgID, input.AccountID, rows)
	if err != nil {
		return nil, err
	}
	h.log.Info("import completed", "job", jobID, "total", total)
	return &StartImportOutput{JobID: jobID, TotalRows: total}, nil
}

// runPipeline executes parsing → mapping for one job, tracking status and
// counters in import_jobs (§8.6 steps a–f). Each stage transition commits
// separately so a crash leaves the job in a truthful state.
func (h *Handler) runPipeline(ctx context.Context, jobID, orgID, accountID uuid.UUID, rows []ParsedRow) (int, error) {
	setStatus := func(status string, errMsg *string) error {
		_, err := h.pool.Exec(ctx, `UPDATE import.import_jobs SET status = $2, total_rows = $3, error_message = $4 WHERE id = $1`,
			jobID, status, len(rows), errMsg)
		return common.Wrap(err)
	}
	if err := setStatus("parsing", nil); err != nil {
		return 0, err
	}

	occurrences := AssignOccurrences(rows)
	stagedRows := make([]stagedRow, len(rows))
	for i, row := range rows {
		stagedRows[i] = stagedRow{rowNumber: i + 1, hash: DedupHash(accountID, row, occurrences[i]), raw: encodeRaw(row)}
	}
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		for _, s := range stagedRows {
			if _, err := tx.Exec(ctx, `INSERT INTO import.raw_rows (import_job_id, row_number, raw_data, dedup_hash, status) VALUES ($1, $2, $3::jsonb, $4, 'pending')`,
				jobID, s.rowNumber, s.raw, s.hash); err != nil {
				return common.Wrap(err)
			}
		}
		return nil
	})
	if err != nil {
		msg := err.Error()
		_ = setStatus("failed", &msg)
		return 0, err
	}

	if err := setStatus("mapping", nil); err != nil {
		return 0, err
	}
	var imported, duplicates int
	for i, row := range rows {
		outcome, mapErr := h.mapOne(ctx, jobID, orgID, accountID, stagedRows[i], row)
		if mapErr != nil {
			h.log.Warn("row mapping failed", "job", jobID, "row", i+1, "err", mapErr)
			if _, err := h.pool.Exec(ctx, `UPDATE import.raw_rows SET status = 'error' WHERE import_job_id = $1 AND row_number = $2`, jobID, i+1); err != nil {
				return 0, common.Wrap(err)
			}
			continue
		}
		if outcome == "duplicate" {
			duplicates++
		} else {
			imported++
		}
	}

	now := time.Now().UTC()
	if _, err := h.pool.Exec(ctx, `UPDATE import.import_jobs SET status = 'completed', imported_rows = $2, duplicate_rows = $3, completed_at = $4 WHERE id = $1`,
		jobID, imported, duplicates, now); err != nil {
		return 0, common.Wrap(err)
	}
	return len(rows), nil
}

// stagedRow is one parsed row staged for mapping, with its dedup identity.
type stagedRow struct {
	rowNumber int
	hash      string
	raw       string
}

// mapOne maps a single staged row: duplicate lookup first (§8.6 step e), then
// MapRow. Returns "mapped" or "duplicate".
func (h *Handler) mapOne(ctx context.Context, jobID, orgID, accountID uuid.UUID, s stagedRow, row ParsedRow) (string, error) {
	var outcome string
	err := db.WithSerializable(ctx, h.pool, func(tx pgx.Tx) error {
		dup, err := IsDuplicate(ctx, tx, accountID, s.hash, jobID)
		if err != nil {
			return err
		}
		if dup {
			if _, err := tx.Exec(ctx, `UPDATE import.raw_rows SET status = 'duplicate' WHERE import_job_id = $1 AND row_number = $2`, jobID, s.rowNumber); err != nil {
				return common.Wrap(err)
			}
			outcome = "duplicate"
			return nil
		}
		txID, err := MapRow(ctx, tx, orgID, accountID, jobID, row)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE import.raw_rows SET status = 'mapped', mapped_transaction_id = $3 WHERE import_job_id = $1 AND row_number = $2`, jobID, s.rowNumber, txID); err != nil {
			return common.Wrap(err)
		}
		outcome = "mapped"
		return nil
	})
	return outcome, err
}
