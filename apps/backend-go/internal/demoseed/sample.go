package demoseed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Sample workbook offered by the first-run checklist (POST /api/onboarding/sample). Unlike the CLI demo it is
// per workspace: its id derives from the workspace id, so two workspaces never collide on the workbooks
// primary key, and "Sample" leads its name so it is never mistaken for the user's own data.
const (
	SampleName        = "Sample: 25 companies (safe to delete)"
	SampleDescription = "Sample data loaded by the first-run checklist: 25 well-known companies with free enrichment columns. Run it to watch the grid fill, then delete it when you are done."
)

// SampleWorkbookID is the stable per-workspace id of the sample workbook.
func SampleWorkbookID(ws string) string {
	sum := sha256.Sum256([]byte(ws))
	return "sample-" + hex.EncodeToString(sum[:])[:16]
}

// SampleResult reports the workbook the call found or created.
type SampleResult struct {
	WorkbookID string
	Created    bool
}

// LoadSample creates the sample workbook and its rows in workspace ws, once. A second (or concurrent) call
// returns the existing workbook with Created false and adds nothing. It starts no run: the user runs it from
// the workbook, so nothing is spent until they choose to.
func LoadSample(ctx context.Context, pool *pgxpool.Pool, ws string) (SampleResult, error) {
	id := SampleWorkbookID(ws)
	res := SampleResult{WorkbookID: id}
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "opengtm-demoseed-sample:"+ws); err != nil {
			return err
		}
		created, err := ensureWorkbook(ctx, tx, ws, id, SampleName, SampleDescription)
		if err != nil {
			return err
		}
		res.Created = created
		return nil
	})
	if err != nil {
		return SampleResult{}, fmt.Errorf("demoseed: load sample: %w", err)
	}
	return res, nil
}
