package workbook

import (
	"context"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// Email Verify column (target_field email_verify).
//
// The Email column's value is a provider observation held in the workbook overlay, not an accepted fact of the lead
// record (CONTEXT.md), so the record's email stays empty and the old mailscout provider, which reads lead.email,
// never had an address to check. The verify column instead takes the row's Email cell as its input through the
// column dependency graph (input_columns, explicit or inferred below), so it runs after the Email column and a
// failed Email cell fails it as an upstream dependency. It asks the verification cascade about that address,
// whatever its provenance (a guessed info@ address is verified like any other, never reported as verified because
// it was found), and stores the cascade's own status.

const (
	verifyTarget = "email_verify"
	// ReasonNoEmailToVerify is the cell error when the row has no email to check.
	reasonNoEmailToVerify = "no_email_to_verify: the Email column has no value in this row"
	reasonVerifierFailed  = "email_verifier_failed: the verification service could not be reached"
)

func (c column) isVerifyColumn() bool {
	return (c.Type == "waterfall" || c.Type == "enrichment") && c.target() == verifyTarget
}

func (c column) isEmailSource() bool {
	return (c.Type == "waterfall" || c.Type == "enrichment") && c.firstTruthy("target_field", "lead_field") == "email"
}

// inferVerifyInputs gives a verify column without input_columns the dependency on the one Email column of the
// workbook. Only the graph copy of the column changes, so the stored configuration is untouched.
func inferVerifyInputs(cols []column) {
	for i := range cols {
		if !cols[i].isVerifyColumn() || manifest.PyTruthy(cols[i].cfg["input_columns"]) {
			continue
		}
		var src []string
		for j := range cols {
			if j != i && cols[j].isEmailSource() {
				src = append(src, cols[j].ID)
			}
		}
		if len(src) == 1 {
			cols[i].g.Set("input_columns", []any{src[0]})
		}
	}
}

// verifyInput is the address a verify column checks: the row's value of the column it depends on (threaded into
// the row by earlier columns of this run), else the lead's own email.
func (rs *runState) verifyInput(lead map[string]any, col column) string {
	for _, ref := range colgraph.Refs(col.g) {
		for _, i := range colgraph.ReferenceIndexes(manifest.PyStrip(ref), rs.allCols) {
			src := rs.cols[i]
			for _, key := range []string{src.ID, src.Name} {
				if s, ok := lead[key].(string); ok && strings.Contains(s, "@") {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	if s, ok := lead["email"].(string); ok && strings.Contains(s, "@") {
		return strings.TrimSpace(s)
	}
	return ""
}

func (rs *runState) verifyColumn(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (cellOutcome, error) {
	email := rs.verifyInput(lead, col)
	if email == "" {
		return rs.commitCell(ctx, col, rowID, leadID, cellResult{Err: reasonNoEmailToVerify})
	}
	vr, err := rs.deps.Verify.Verify(ctx, rs.p.WorkspaceID, email)
	if err != nil {
		if ctx.Err() != nil {
			return cellOutcome{}, context.Cause(ctx)
		}
		rs.log.Warn("email verify failed", "col", col.ID, "row", rowID)
		return rs.commitCell(ctx, col, rowID, leadID, cellResult{Err: reasonVerifierFailed})
	}
	provider := vr.Source
	if provider == "" {
		provider = "email_verify"
	}
	r := cellResult{Has: true, Value: vr.Status, Provider: provider,
		Meta: map[string]any{"verify": map[string]any{"status": vr.Status, "confidence": vr.Confidence, "source": vr.Source, "email": email}}}
	if vr.Confidence != 0 {
		c := vr.Confidence
		r.Confidence = &c
	}
	return rs.commitCell(ctx, col, rowID, leadID, r)
}
