package api

import (
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func TestRegisterAllWithoutPGLeadStoreRegistersNoWorkbookRoutes(t *testing.T) {
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Flags: config.Flags{PGLeadStore: false}}
	if d.workbooksEnabled("workbooks") {
		t.Fatal("workbooksEnabled must be false without PG_LEAD_STORE")
	}
	r := httpx.NewRouter()
	RegisterAll(r, d)
	for _, p := range r.Patterns() {
		if strings.Contains(p, "/api/workbooks") || strings.Contains(p, "/api/v2/workbooks") {
			t.Errorf("workbook route registered without PG_LEAD_STORE: %s", p)
		}
	}
	d.Flags.PGLeadStore = true
	if !d.workbooksEnabled("workbooks") {
		t.Fatal("workbooksEnabled must be true with PG_LEAD_STORE")
	}
}

func TestSchemaContainsWorkbookTables(t *testing.T) {
	b, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^CREATE TABLE public\.(\w+) `).FindAllStringSubmatch(string(b), -1) {
		have[m[1]] = true
	}
	for _, tbl := range []string{
		"workbooks", "workbook_rows", "workbook_views", "workbook_enrichments", "connector_runs",
		"workbook_spend_attempts", "workbook_activity", "cell_traces", "company_entities",
		"entity_review_pairs", "entity_merge_log", "leads", "jobs", "workbook_functions",
	} {
		if !have[tbl] {
			t.Errorf("schema.sql lacks table %s", tbl)
		}
	}
}
