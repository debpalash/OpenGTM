//go:build integration

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// pgWorkbooks is the test-local WorkbookPort: it writes the same row the REST
// create inserts (wbcrud.NewID, gen.InsertWorkbook, source "empty") inside the
// pipeline transaction, and deletes through wbcrud's ownership check. The
// production adapter is Task 17's.
type pgWorkbooks struct{ svc wbcrud.Service }

func (p pgWorkbooks) Create(ctx context.Context, tx pgx.Tx, ws, name, desc string, cols []Column) (string, error) {
	colJSON, err := json.Marshal(cols)
	if err != nil {
		return "", err
	}
	row, err := gen.New(tx).InsertWorkbook(ctx, gen.InsertWorkbookParams{
		ID: wbcrud.NewID(), Name: name, Description: desc, WorkspaceID: ws, SourceType: "empty",
		SourceConfig: []byte("{}"), FilterCriteria: []byte("{}"), ColumnsConfig: colJSON,
	})
	if err != nil {
		return "", err
	}
	return row.ID, nil
}

func (p pgWorkbooks) Delete(ctx context.Context, tx pgx.Tx, ws, id string) (bool, error) {
	if _, err := p.svc.Owned(ctx, tx, ws, id, false); err != nil {
		if errors.Is(err, wbcrud.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	n, err := gen.New(tx).DeleteWorkbook(ctx, gen.DeleteWorkbookParams{WorkbookID: id, WorkspaceID: ws})
	return n == 1, err
}

func (e *wEnv) wbServer() *Server {
	d := e.d
	d.Cfg = e.cfg
	d.Workbooks = pgWorkbooks{}
	return NewServer(d, NewRegistry(nil, workbookWriteTools(d)))
}

func (e *wEnv) wbExists(t *testing.T, ws, id string) int {
	t.Helper()
	var n int
	if err := e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM workbooks WHERE id=$1`, id).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *wEnv) seedWB(t *testing.T, ws, id string) {
	t.Helper()
	if err := e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1,'n',$2)`, id, ws)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func wbIDOf(t *testing.T, text string) string {
	t.Helper()
	var r struct {
		ID string `json:"workbook_id"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil || r.ID == "" {
		t.Fatalf("no workbook_id in %s", text)
	}
	return r.ID
}

func TestCreateWorkbookStoredColumnsByteIdentical(t *testing.T) {
	e := newWEnv(t, "editor")
	s := e.wbServer()
	// Fixture-recorded stored columns_config of the Python write.
	raw, err := os.ReadFile("../../testdata/mcp/tools/writes__create_workbook_idempotent_replay.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Effects struct {
			Workbooks struct {
				Added []struct {
					Columns json.RawMessage `json:"columns_config"`
				} `json:"added"`
			} `json:"workbooks"`
		} `json:"effects"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Effects.Workbooks.Added) != 1 {
		t.Fatal("fixture", err)
	}
	out := e.call(s, "create_workbook", Args{"description": "x", "idempotency_key": "k-wb-1"})
	if out != fixtureTextWithID(t, "writes__create_workbook_idempotent_replay.json", wbIDOf(t, out)) {
		t.Fatalf("result %s", out)
	}
	var stored, want string
	if err := e.d.RunInWorkspace(context.Background(), e.ws, func(tx pgx.Tx) error {
		if err := tx.QueryRow(context.Background(),
			`SELECT columns_config::text FROM workbooks WHERE id=$1`, wbIDOf(t, out)).Scan(&stored); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT $1::json::text`, string(f.Effects.Workbooks.Added[0].Columns)).Scan(&want)
	}); err != nil {
		t.Fatal(err)
	}
	// columns_config is a json column: it keeps whitespace and key order as sent.
	// Python's json.dumps adds ", " and ": "; the Go port stores compact JSON
	// (as the REST create does). Compare the compacted bytes: key order and
	// every value must be identical.
	var cs, cw bytes.Buffer
	if json.Compact(&cs, []byte(stored)) != nil || json.Compact(&cw, []byte(want)) != nil {
		t.Fatal("compact")
	}
	if cs.String() != cw.String() {
		t.Fatalf("stored %s\nwant   %s", stored, want)
	}
	var name, st, sc, fc string
	_ = e.d.RunInWorkspace(context.Background(), e.ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT name, source_type, source_config::text, filter_criteria::text FROM workbooks WHERE id=$1`, wbIDOf(t, out)).Scan(&name, &st, &sc, &fc)
	})
	if name != "MCP — x" || st != "empty" || sc != "{}" || fc != "{}" {
		t.Fatalf("row %q %q %q %q", name, st, sc, fc)
	}
	// replay does not create a second workbook
	if again := e.call(s, "create_workbook", Args{"description": "x", "idempotency_key": "k-wb-1"}); !strings.Contains(again, "duplicate") {
		t.Fatalf("replay %s", again)
	}
	if e.workbooks(t) != 1 {
		t.Fatalf("workbooks %d", e.workbooks(t))
	}
}

func fixtureTextWithID(t *testing.T, file, id string) string {
	t.Helper()
	want := fixtureText(t, file)
	return strings.ReplaceAll(want, wbIDOf(t, want), id)
}

func TestCreateWorkbookMissingDescriptionAudited(t *testing.T) {
	e := newWEnv(t, "editor")
	s := e.wbServer()
	if got := e.call(s, "create_workbook", Args{}); got != `{"error": "description is required"}` {
		t.Fatal(got)
	}
	if e.workbooks(t) != 0 || e.held(t) != 0 {
		t.Fatal("state left behind")
	}
	e.wantAudit(t, 1, "error", "description is required")
}

func TestDeleteWorkbookHardDeleteAndIdempotent(t *testing.T) {
	e := newWEnv(t, "admin")
	s := e.wbServer()
	e.seedWB(t, e.ws, e.ws+"/del")
	want := `{"status": "deleted", "workbook_id": "` + e.ws + `/del"}`
	if got := e.call(s, "delete_workbook", Args{"workbook_id": e.ws + "/del"}); got != want {
		t.Fatalf("got %s", got)
	}
	if e.wbExists(t, e.ws, e.ws+"/del") != 0 {
		t.Fatal("row survived: not a hard delete")
	}
	again := e.call(s, "delete_workbook", Args{"workbook_id": e.ws + "/del"})
	if again != `{"status": "not_found", "workbook_id": "`+e.ws+`/del"}` {
		t.Fatalf("got %s", again)
	}
	if got := e.call(s, "delete_workbook", Args{}); got != `{"error": "workbook_id is required"}` {
		t.Fatalf("got %s", got)
	}
	if e.held(t) != 0 {
		t.Fatal("held reservation left")
	}
}

func TestDeleteWorkbookCrossTenantNotFound(t *testing.T) {
	e := newWEnv(t, "admin")
	s := e.wbServer()
	other := e.workspace(t, "o", &e.uid, "")
	id := "t12-foreign-" + e.sfx
	e.seedWB(t, other, id)
	t.Cleanup(func() {
		_ = e.d.RunInWorkspace(context.Background(), other, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `DELETE FROM workbooks WHERE workspace_id=$1`, other)
			return err
		})
	})
	got := e.call(s, "delete_workbook", Args{"workbook_id": id, "workspace_id": other})
	if got != `{"status": "not_found", "workbook_id": "`+id+`"}` {
		t.Fatalf("got %s", got)
	}
	if e.wbExists(t, other, id) != 1 {
		t.Fatal("other workspace's workbook was deleted")
	}
}

func TestWorkbookToolsRequireCapabilityAndRoleThroughPipeline(t *testing.T) {
	// editor cannot delete (admin floor); viewer cannot create; a token without
	// the capability is refused before anything runs.
	e := newWEnv(t, "editor")
	s := e.wbServer()
	e.seedWB(t, e.ws, e.ws+"/keep")
	if got := e.call(s, "delete_workbook", Args{"workbook_id": e.ws + "/keep"}); got != `{"error": "insufficient workspace role"}` {
		t.Fatalf("got %s", got)
	}
	if e.wbExists(t, e.ws, e.ws+"/keep") != 1 {
		t.Fatal("editor deleted a workbook")
	}
	e.wantAudit(t, 1, "denied", "insufficient workspace role")

	noCap := e.ctx
	noCap.Capabilities = map[string]bool{CapLeadsRead: true}
	if got := s.callTool(context.Background(), noCap, "create_workbook", Args{"description": "x"}); !strings.Contains(got, "error") {
		t.Fatalf("got %s", got)
	}
	if e.workbooks(t) != 1 {
		t.Fatal("create ran without the capability")
	}

	if got := e.call(s, "create_workbook", Args{"description": "ok"}); !strings.Contains(got, `"created"`) {
		t.Fatalf("editor create: %s", got)
	}
}

func TestViewerDowngradedCannotCreateWorkbook(t *testing.T) {
	v := newWEnv(t, "viewer")
	if got := v.call(v.wbServer(), "create_workbook", Args{"description": "x"}); got != fixtureText(t, "writes__role_viewer_downgraded_create_workbook.json") {
		t.Fatalf("got %s", got)
	}
	if v.workbooks(t) != 0 {
		t.Fatal("viewer created a workbook")
	}
}
