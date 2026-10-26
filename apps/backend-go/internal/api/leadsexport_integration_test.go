//go:build integration

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func exportActor(name string) (who, ws string) {
	switch name {
	case "export_no_auth":
		return "", ""
	case "export_beta":
		return "bob", "ws-beta"
	}
	return "alice", ""
}

func TestExportBytesMatchPython(t *testing.T) {
	e := newLeadsEnv(t)
	n := 0
	for _, full := range contractFixtureNames(t, "export") {
		name := strings.TrimPrefix(full, "export/")
		fx := contract.Load(t, full)
		who, ws := exportActor(name)
		rec := e.get(who, ws, fx.Request.Path)
		var body json.RawMessage
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
			body, _ = json.Marshal(rec.Body.String())
			if cd := rec.Header().Get("Content-Disposition"); cd != "attachment; filename=opengtm_leads.csv" {
				t.Errorf("%s: Content-Disposition %q", name, cd)
			}
		} else {
			body = json.RawMessage(rec.Body.Bytes())
		}
		got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: body}
		for _, k := range []string{"Content-Type", "WWW-Authenticate"} {
			if v := rec.Header().Get(k); v != "" {
				got.Headers[k] = v
			}
		}
		n++
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	if n != 7 {
		t.Fatalf("replayed %d fixtures", n)
	}
}

func (e leadsEnv) exec(t *testing.T, ws, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExportFormulaCellsNeutralised(t *testing.T) {
	e := newLeadsEnv(t)
	e.exec(t, "ws-alpha", `INSERT INTO leads (id, workspace_id, company, city, phone, notes, score, status, source)
		VALUES (9001, 'ws-alpha', '=HYPERLINK("http://x")', 'c', '+91 (22) 123-4567', '@SUM(A1)', 999, 'new', 'x')`)
	body := e.get("alice", "ws-alpha", "/api/export/csv").Body.String()
	for _, want := range []string{`"'=HYPERLINK(""http://x"")"`, ",+91 (22) 123-4567,", ",'@SUM(A1)\r\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in export", want)
		}
	}
	if strings.Contains(body, "\n=HYPERLINK") {
		t.Error("raw formula cell")
	}
}

func TestExportFiltersRaw(t *testing.T) {
	e := newLeadsEnv(t)
	all := e.get("alice", "ws-alpha", "/api/export/csv?city=__all__")
	if all.Code != 200 || strings.Count(all.Body.String(), "\r\n") != 1 {
		t.Errorf("__all__ is a literal city (matches nothing): %d %q", all.Code, all.Body.String())
	}
}

func TestExportCapped10000(t *testing.T) {
	e := newLeadsEnv(t)
	e.exec(t, "ws-alpha", `INSERT INTO leads (workspace_id, company, city, score, status, source)
		SELECT 'ws-alpha', 'bulk ' || g, 'bulkcity', 1, 'new', 'x' FROM generate_series(1, 10050) g`)
	body := e.get("alice", "ws-alpha", "/api/export/csv?city=bulkcity").Body.String()
	if n := strings.Count(body, "\r\n"); n != 10001 {
		t.Errorf("%d lines, want header + 10000", n)
	}
}

func TestExportTenantIsolation(t *testing.T) {
	e := newLeadsEnv(t)
	body := e.get("bob", "ws-beta", "/api/export/csv").Body.String()
	alpha := e.get("alice", "ws-alpha", "/api/export/csv").Body.String()
	if body == alpha || strings.Contains(body, "Berlin Bakery") && !strings.Contains(alpha, "Berlin Bakery") {
		t.Errorf("workspaces share rows")
	}
	if rec := e.get("alice", "ws-beta", "/api/export/csv"); rec.Code != 403 {
		t.Errorf("foreign workspace: %d", rec.Code)
	}
	for _, l := range strings.Split(alpha, "\r\n")[1:] {
		if strings.Contains(body, l) && l != "" {
			t.Errorf("alpha row leaked into beta export: %q", l)
		}
	}
}
