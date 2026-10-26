package workbook

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

var (
	leadFieldsOnce sync.Once
	leadFieldSet   map[string]bool
)

// isLeadField is `field in LEAD_FIELD_MAP`.
func isLeadField(f string) bool {
	leadFieldsOnce.Do(func() {
		leadFieldSet = map[string]bool{}
		if reg, err := wbregistry.Load(); err == nil {
			for _, k := range reg.LeadFields() {
				leadFieldSet[k] = true
			}
		}
	})
	return leadFieldSet[f]
}

func writeText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return manifest.PyStr(v)
}

// writeBackFields ports the result write-back of enrich_cell: every non-empty
// field of a successful provider result is written to the linked lead (the
// source of truth) through the plan 05 write store: structured fields, any
// other known lead field except the cell's own target, and a structured JSON
// blob target. Like Python each field is its own best-effort write: a value the
// store rejects is logged and skipped, never failing the cell. Only a lost lease
// or a cancelled job aborts the run.
func (rs *runState) writeBackFields(ctx context.Context, lead map[string]any, fields map[string]any, target, provider string) error {
	lv, present := lead["__lead_id"]
	if !present || lv == nil {
		return nil // v2 row without a lead: nothing to write back to
	}
	leadID := intOf(lv)
	if leadID <= 0 {
		return nil
	}
	type write struct {
		field string
		upd   map[string]any
	}
	var writes []write
	for _, name := range sortedKeys(fields) {
		v := fields[name]
		if !manifest.PyTruthy(v) || v == "N/A" {
			continue
		}
		switch {
		case structuredFields[name]:
		case isLeadField(name) && name != target:
		default:
			continue
		}
		writes = append(writes, write{name, withProvider(name, writeText(v), provider)})
	}
	if tv, ok := fields[target]; ok && manifest.PyTruthy(tv) && tv != "N/A" {
		if sv := manifest.PyStr(tv); strings.HasPrefix(sv, "[{") || strings.HasPrefix(sv, `{"`) {
			writes = append(writes, write{target, withProvider(target, writeText(tv), provider)})
		}
	}
	for _, w := range writes {
		err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
			if err := fence(ctx, tx, rs.claim, rs.p.WorkbookID); err != nil {
				return err
			}
			_, err := leadswrite.UpdateFields(ctx, tx, leadID, w.upd)
			return err
		})
		if err != nil {
			if a := abortErr(ctx, err); a != nil {
				return a
			}
			rs.log.Warn("failed to write back to lead", "lead", leadID, "field", w.field)
		}
	}
	return nil
}

func withProvider(field, text, provider string) map[string]any {
	upd := map[string]any{field: text}
	if provider != "" {
		switch field {
		case "email":
			upd["email_provider"] = provider
		case "phone":
			upd["phone_provider"] = provider
		}
	}
	return upd
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
