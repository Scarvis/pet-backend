// Deduplication hashes (§8.6 step c): SHA256(account + FITID) when the file
// carries a bank transaction id (OFX), otherwise SHA256 of account + date +
// amount + description + occurrence index. The occurrence index is the ordinal
// (0, 1, 2, …) of the identical row within the file, so two genuine identical
// purchases on the same day do not collapse and do not violate
// UNIQUE(import_job_id, dedup_hash). Cross-job duplicates are found by lookup
// (IsDuplicate), since the UNIQUE constraint only guards within one job.
package imports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Scarvis/pet-backend/internal/common"
)

// occurrenceKey groups identical rows within one file for the occurrence index.
func occurrenceKey(row ParsedRow) string {
	return strings.Join([]string{
		row.OccurredAt.UTC().Format("2006-01-02"),
		strings.TrimSpace(row.AmountDecimal),
		strings.TrimSpace(row.Description),
	}, "\x00")
}

// AssignOccurrences numbers each row's ordinal among identical rows in the
// file, preserving input order.
func AssignOccurrences(rows []ParsedRow) []int {
	seen := map[string]int{}
	out := make([]int, len(rows))
	for i, row := range rows {
		key := occurrenceKey(row)
		out[i] = seen[key]
		seen[key]++
	}
	return out
}

// DedupHash builds the row hash per §8.6. fitid takes precedence; otherwise
// the occurrence index disambiguates identical rows.
func DedupHash(accountID uuid.UUID, row ParsedRow, occurrence int) string {
	var raw string
	if row.FITID != "" {
		raw = accountID.String() + "|" + strings.TrimSpace(row.FITID)
	} else {
		raw = fmt.Sprintf("%s|%s|%s|%s|%d",
			accountID.String(),
			row.OccurredAt.UTC().Format("2006-01-02"),
			strings.TrimSpace(row.AmountDecimal),
			strings.TrimSpace(row.Description),
			occurrence)
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// IsDuplicate reports whether hash already appears in raw_rows of an earlier
// job for the same account with status 'mapped' (§8.6 step e).
func IsDuplicate(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, hash string, jobID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM import.raw_rows r
		JOIN import.import_jobs j ON j.id = r.import_job_id
		WHERE j.account_id = $1 AND r.dedup_hash = $2 AND r.status = 'mapped' AND r.import_job_id <> $3
	)`, accountID, hash, jobID).Scan(&exists)
	if err != nil {
		return false, common.Wrap(err)
	}
	return exists, nil
}
