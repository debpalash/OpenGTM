//go:build integration

package crmpush_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type dbEnv struct {
	app   *pgxpool.Pool
	owner *pgxpool.Pool
	ws    [2]string
	wb    [2]string
}

func newDBEnv(t *testing.T) *dbEnv {
	t.Helper()
	app := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	e := &dbEnv{app: app, owner: owner, ws: [2]string{"p07t11-ws-a", "p07t11-ws-b"}, wb: [2]string{"p07t11-wb-a", "p07t11-wb-b"}}
	clean := func() {
		for i := range e.ws {
			_, _ = owner.Exec(ctx, `DELETE FROM workbook_rows WHERE workbook_id=$1`, e.wb[i])
			_, _ = owner.Exec(ctx, `DELETE FROM workbooks WHERE id=$1`, e.wb[i])
			_, _ = owner.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, e.ws[i])
		}
	}
	clean()
	t.Cleanup(clean)
	for i := range e.ws {
		if _, err := owner.Exec(ctx, `INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ($1::text,$1::text,$1::text, extract(epoch from clock_timestamp()), extract(epoch from now()))`, e.ws[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1,$1,$2)`, e.wb[i], e.ws[i]); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// row inserts a row as owner and returns its id.
func (e *dbEnv) row(t *testing.T, side int, leadID any, data string) int64 {
	t.Helper()
	var id int64
	if err := e.owner.QueryRow(context.Background(),
		`INSERT INTO workbook_rows (workspace_id, workbook_id, "position", data, enrichments, lead_id) VALUES ($1,$2,0,$3::json,'{}'::json,$4) RETURNING id`,
		e.ws[side], e.wb[side], data, leadID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *dbEnv) data(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := e.owner.QueryRow(context.Background(), `SELECT data::text FROM workbook_rows WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *dbEnv) clear() {
	for i := range e.wb {
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM workbook_rows WHERE workbook_id=$1`, e.wb[i])
	}
}

type rowIDVec struct {
	Name string `json:"name"`
	Rows []struct {
		LeadID *int64          `json:"lead_id"`
		Data   json.RawMessage `json:"data"`
	} `json:"rows"`
	Args struct {
		LeadID json.RawMessage `json:"lead_id"`
		CRM    string          `json:"crm_type"`
		Old    string          `json:"old"`
		New    string          `json:"new"`
		Object string          `json:"object"`
	} `json:"args"`
	Returns   bool              `json:"returns"`
	FinalData []json.RawMessage `json:"final_data"`
}

func TestStoreRowCRMIDVectors(t *testing.T) {
	e := newDBEnv(t)
	raw, err := os.ReadFile("testdata/crmpush.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		RowID []rowIDVec `json:"row_id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.RowID) < 10 {
		t.Fatalf("only %d vectors", len(v.RowID))
	}
	for _, c := range v.RowID {
		t.Run(c.Name, func(t *testing.T) {
			e.clear()
			var ids []int64
			for _, r := range c.Rows {
				var lid any
				if r.LeadID != nil {
					lid = *r.LeadID
				}
				ids = append(ids, e.row(t, 0, lid, string(r.Data)))
			}
			var leadID int64
			if bytes.HasPrefix(c.Args.LeadID, []byte("{")) { // {"row_index": n}
				var ri struct {
					RowIndex int `json:"row_index"`
				}
				_ = json.Unmarshal(c.Args.LeadID, &ri)
				leadID = ids[ri.RowIndex]
			} else {
				_ = json.Unmarshal(c.Args.LeadID, &leadID)
			}
			ok, err := crmpush.StoreRowCRMID(context.Background(), e.app, e.ws[0], e.wb[0], leadID, c.Args.CRM, c.Args.Old, c.Args.New, c.Args.Object)
			if err != nil || ok != c.Returns {
				t.Fatalf("ok=%v err=%v want %v", ok, err, c.Returns)
			}
			for i, want := range c.FinalData {
				got := e.data(t, ids[i])
				if !sameOrdered(t, got, string(want)) {
					t.Fatalf("row %d data %s want %s", i, got, want)
				}
			}
		})
	}
}

// sameOrdered compares two JSON documents including top-level key order.
func sameOrdered(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y) && reflect.DeepEqual(orderedKeys(t, []byte(a)), orderedKeys(t, []byte(b)))
}

func hsDeps(e *dbEnv, hs *fakeHS) (crmpush.Deps, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	return crmpush.Deps{HubSpot: hs, Pool: e.app, Log: slog.New(slog.NewTextHandler(logs, nil))}, logs
}

func TestFreshIDStampedOnlyIfStaleKeyMatches(t *testing.T) {
	e := newDBEnv(t)
	hs := &fakeHS{connected: true,
		update: crm.UpdateResult{NotFound: true},
		push:   crm.PushResult{Success: true, Action: "created", HubspotID: "901"}}
	d, _ := hsDeps(e, hs)
	ctx := context.Background()

	// matching stale key: stamped, key order kept, other keys intact
	stale := `{"company": "Acme", "crm_external_id": "501", "crm_type": "hubspot", "crm_object": "contact", "nested": {"a": [1, 2]}}`
	id := e.row(t, 0, 7, stale)
	lead := map[string]any{"email": "a@x.com", "crm_external_id": "501", "crm_type": "hubspot"}
	r := crmpush.Push(ctx, d, e.ws[0], nil, lead, e.wb[0], 7)
	if !r.Success || r.Value != "HubSpot: created 901" {
		t.Fatalf("%+v", r)
	}
	want := `{"company": "Acme", "crm_external_id": "901", "crm_type": "hubspot", "crm_object": "contact", "nested": {"a": [1, 2]}}`
	if got := e.data(t, id); !sameOrdered(t, got, want) {
		t.Fatalf("stamped data %s", got)
	}

	// the row now carries a different (fresh) id: a second fallback with the old stale key must not overwrite it
	r = crmpush.Push(ctx, d, e.ws[0], nil, lead, e.wb[0], 7)
	if !r.Success {
		t.Fatalf("%+v", r)
	}
	if got := e.data(t, id); !sameOrdered(t, got, want) {
		t.Fatalf("row with a different key was overwritten: %s", got)
	}

	// no key on the row at all: untouched
	e.clear()
	bare := `{"company": "Acme"}`
	id2 := e.row(t, 0, 7, bare)
	_ = crmpush.Push(ctx, d, e.ws[0], nil, lead, e.wb[0], 7)
	if got := e.data(t, id2); !sameOrdered(t, got, bare) {
		t.Fatalf("keyless row modified: %s", got)
	}
}

func TestCrossWorkspaceRowNotStamped(t *testing.T) {
	e := newDBEnv(t)
	// The same ids and keys exist in workspace B's workbook.
	foreign := e.row(t, 1, 7, `{"crm_external_id": "501", "crm_type": "hubspot", "crm_object": "contact"}`)
	before := e.data(t, foreign)
	ctx := context.Background()

	// 1. workspace A asks to stamp workspace B's workbook
	ok, err := crmpush.StoreRowCRMID(ctx, e.app, e.ws[0], e.wb[1], 7, "hubspot", "501", "901", "contact")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	// 2. same through Push
	hs := &fakeHS{connected: true, update: crm.UpdateResult{NotFound: true}, push: crm.PushResult{Success: true, Action: "created", HubspotID: "901"}}
	d, _ := hsDeps(e, hs)
	r := crmpush.Push(ctx, d, e.ws[0], nil, map[string]any{"email": "a@x.com", "crm_external_id": "501", "crm_type": "hubspot"}, e.wb[1], 7)
	if !r.Success {
		t.Fatalf("push result must stay successful: %+v", r)
	}
	if got := e.data(t, foreign); got != before {
		t.Fatalf("foreign row stamped: %s", got)
	}
}

func TestStampFailureDoesNotFailPushOnDB(t *testing.T) {
	e := newDBEnv(t)
	e.row(t, 0, 7, `{"crm_external_id": "501", "crm_type": "hubspot"}`)
	hs := &fakeHS{connected: true, update: crm.UpdateResult{NotFound: true}, push: crm.PushResult{Success: true, Action: "created", HubspotID: "901"}}
	// a pool that was closed: every stamp attempt errors
	dead, err := pgxpool.NewWithConfig(context.Background(), e.app.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	logs := &bytes.Buffer{}
	d := crmpush.Deps{HubSpot: hs, Pool: dead, Log: slog.New(slog.NewTextHandler(logs, nil))}
	r := crmpush.Push(context.Background(), d, e.ws[0], nil, map[string]any{"email": "a@x.com", "crm_external_id": "501", "crm_type": "hubspot"}, e.wb[0], 7)
	if !r.Success || r.Value != "HubSpot: created 901" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("failure not logged: %q", logs.String())
	}
}
