// Package spend ports the Python workbook spend ledger
// (apps/api/services/workbook/spend_service.py) so Go and Python executors
// share attempt keys, contract hashes, and settlement semantics.
package spend

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// ErrInvalidInput marks caller errors that Python raises as ValueError.
var ErrInvalidInput = errors.New("spend: invalid input")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, a...))
}

// AttemptKey ports WorkbookExecutionIdentity.attempt_key: sha256 over the
// compact, unsorted, ASCII-escaped JSON list
// [workspace, workbook, "job:<id>", row, column, provider]. It returns ""
// (which Reserve rejects) when a row/column/provider identity is empty or the
// job id is not positive, mirroring Python's ValueError.
func AttemptKey(workspaceID, workbookID string, jobID int64, rowIdentity, columnID, provider string) string {
	if rowIdentity == "" || columnID == "" || provider == "" || jobID <= 0 {
		return ""
	}
	enc, err := canonjson.Marshal([]any{workspaceID, workbookID, fmt.Sprintf("job:%d", jobID),
		rowIdentity, columnID, provider}, false)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(enc)
	return hex.EncodeToString(sum[:])
}

// Reservation is the input to Reserve (reserve_attempt's keyword arguments).
type Reservation struct {
	WorkspaceID, WorkbookID, RunID, RowIdentity, ColumnID, Provider, AttemptKey string
	ExposureMicroUSD, CellLimitMicroUSD                                         int64
	CostBasis, Operation                                                        map[string]any
}

// ContractHash returns the canonical contract JSON and its sha256 hex digest
// exactly as reserve_attempt computes them.
func ContractHash(r Reservation) (serialized, digest string, err error) {
	b, err := canonjson.Marshal(map[string]any{
		"workbook_id": r.WorkbookID, "run_id": r.RunID, "row_identity": r.RowIdentity,
		"column_id": r.ColumnID, "provider": r.Provider, "exposure": r.ExposureMicroUSD,
		"cell_limit": r.CellLimitMicroUSD, "cost_basis": nilToNull(r.CostBasis), "operation": nilToNull(r.Operation),
	}, true)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:]), nil
}

func nilToNull(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

func (r Reservation) validate() error {
	for _, v := range []string{r.WorkspaceID, r.WorkbookID, r.RunID, r.RowIdentity, r.ColumnID, r.Provider, r.AttemptKey} {
		if n := utf8.RuneCountInString(v); n == 0 || n > 255 {
			return invalid("bounded scoped identities are required")
		}
	}
	if r.ExposureMicroUSD < 0 || r.CellLimitMicroUSD < 0 {
		return invalid("exposure and cell limit must be nonnegative integer micro-USD")
	}
	if len(r.CostBasis) == 0 {
		return invalid("an explicit cost basis is required")
	}
	if len(r.Operation) == 0 {
		return invalid("an explicit provider operation contract is required")
	}
	return nil
}

// LeaseFence identifies the queue lease a dispatch must still hold.
type LeaseFence struct {
	JobID      int64
	WorkerID   string
	LockedAt   time.Time
	LeaseToken int64
}

// Identity scopes an existing attempt (transition_attempt/settle_attempt args).
// Lease, when set, fences "dispatch" against the live run_workbook job and
// restricts it to attempts whose run_id is "job:<JobID>".
type Identity struct {
	WorkspaceID, WorkbookID, AttemptID, ContractHash string
	Lease                                            *LeaseFence
}

func (i Identity) validate() error {
	for _, v := range []string{i.WorkspaceID, i.WorkbookID, i.AttemptID, i.ContractHash} {
		if v == "" {
			return invalid("scoped attempt identity and contract hash are required")
		}
	}
	return nil
}
