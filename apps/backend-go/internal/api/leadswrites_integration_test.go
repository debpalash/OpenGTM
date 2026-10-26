//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// newWriteEnv adds the lead id sequence fix: seedLeads inserts ids 1-18 explicitly, so the serial is still at 1.
func newWriteEnv(t *testing.T) leadsEnv {
	t.Helper()
	e := newLeadsEnv(t)
	ctx := context.Background()
	owner, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	if _, err := owner.Exec(ctx, `SELECT setval(pg_get_serial_sequence('leads','id'), 1000)`); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e leadsEnv) send(who, ws, method, path, body string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do(method, path, "application/json", body, hdr)
	return rec
}

func leadRow(t *testing.T, e leadsEnv, ws string, id int, cols string) map[string]any {
	t.Helper()
	var raw []byte
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT to_jsonb(l) - 'search_tsv' FROM leads l WHERE id = $1`, id).Scan(&raw)
	})
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func TestAddLeadCreatesAndReturnsId(t *testing.T) {
	e := newWriteEnv(t)
	rec := e.send("alice", "ws-alpha", "POST", "/api/lead", `{"company":"Fresh Co","city":"Oslo"}`)
	var out struct {
		OK bool `json:"ok"`
		ID int  `json:"id"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.OK || out.ID < 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Body.String(), `{"ok":true,"id":`) {
		t.Errorf("key order: %s", rec.Body.String())
	}
	row := leadRow(t, e, "ws-alpha", out.ID, "")
	if row["company"] != "Fresh Co" || row["source"] != "manual" || row["status"] != "new" || row["workspace_id"] != "ws-alpha" {
		t.Errorf("row %v", row)
	}
}

func TestAddLeadConflict409Shape(t *testing.T) {
	e := newWriteEnv(t)
	before := leadRow(t, e, "ws-alpha", 6, "")
	rec := e.send("alice", "ws-alpha", "POST", "/api/lead", `{"company":"Zenith Robotics Pvt Ltd","city":"Mumbai","email":"new@x.example.com"}`)
	want := `{"detail":{"message":"A lead already exists for this company and city. Update the existing lead instead.","lead_id":6}}`
	if rec.Code != 409 || rec.Body.String() != want {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	after := leadRow(t, e, "ws-alpha", 6, "")
	if before["email"] != after["email"] || before["updated_at"] != after["updated_at"] {
		t.Error("collision modified the row")
	}
}

func TestAddLeadRaceExactlyOneWinner(t *testing.T) {
	e := newWriteEnv(t)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	bodies := make([]string, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := e.send("alice", "ws-alpha", "POST", "/api/lead", `{"company":"Race Co","city":""}`)
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}()
	}
	wg.Wait()
	ok := 0
	for i, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
		default:
			t.Errorf("%d: %d %s", i, c, bodies[i])
		}
	}
	if ok != 1 {
		t.Errorf("%d winners: %v", ok, codes)
	}
}

func TestAddLeadMissingCompany422(t *testing.T) {
	e := newWriteEnv(t)
	for _, b := range []string{`{"city":"Oslo"}`, `{"company":5}`, `{"company":"x","city":null}`, `[1]`, ``, `{bad`} {
		if rec := e.send("alice", "ws-alpha", "POST", "/api/lead", b); rec.Code != 422 {
			t.Errorf("%q: %d %s", b, rec.Code, rec.Body.String())
		}
	}
}

func TestAddLeadNULByte422(t *testing.T) {
	e := newWriteEnv(t)
	rec := e.send("alice", "ws-alpha", "POST", "/api/lead", `{"company":"a\u0000b"}`)
	if rec.Code != 422 {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestPutLeadUpdatesAllowedFields(t *testing.T) {
	e := newWriteEnv(t)
	before := leadRow(t, e, "ws-alpha", 10, "")
	rec := e.send("alice", "ws-alpha", "PUT", "/api/lead/10", `{"phone":"+1 555","notes":"n","score":77}`)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	row := leadRow(t, e, "ws-alpha", 10, "")
	if row["phone"] != "+1 555" || row["notes"] != "n" || row["score"] != float64(77) {
		t.Errorf("row %v", row)
	}
	if row["updated_at"] == before["updated_at"] {
		t.Error("updated_at did not advance")
	}
}

func TestPutLeadIgnoresIdAndWorkspaceId(t *testing.T) {
	e := newWriteEnv(t)
	before := leadRow(t, e, "ws-alpha", 10, "")
	rec := e.send("alice", "ws-alpha", "PUT", "/api/lead/10", `{"id":999,"workspace_id":"ws-beta","created_at":"1999-01-01","search_tsv":"x","not_a_column":1}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	row := leadRow(t, e, "ws-alpha", 10, "")
	if row == nil || row["workspace_id"] != "ws-alpha" || row["created_at"] != before["created_at"] || row["company"] != before["company"] {
		t.Errorf("row %v", row)
	}
	if leadRow(t, e, "ws-beta", 999, "") != nil || leadRow(t, e, "ws-alpha", 999, "") != nil {
		t.Error("id was writable")
	}
}

func TestPutLeadWrongType422(t *testing.T) {
	e := newWriteEnv(t)
	before := leadRow(t, e, "ws-alpha", 10, "")
	for _, b := range []string{`{"notes":"changed","score":"abc"}`, `{"notes":"changed","company":null}`, `{"notes":"changed","phone":"a\u0000b"}`, `{"notes":"changed","score":{"a":1}}`} {
		rec := e.send("alice", "ws-alpha", "PUT", "/api/lead/10", b)
		if rec.Code != 422 {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
	after := leadRow(t, e, "ws-alpha", 10, "")
	if after["notes"] != before["notes"] || after["updated_at"] != before["updated_at"] {
		t.Error("partial write on rejected update")
	}
}

func TestPutLeadNonObjectBody422(t *testing.T) {
	e := newWriteEnv(t)
	rec := e.send("alice", "ws-alpha", "PUT", "/api/lead/10", `[1,2]`)
	if rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestStatusInvalid400(t *testing.T) {
	e := newWriteEnv(t)
	rec := e.send("alice", "ws-alpha", "POST", "/api/lead/14/status", `{"status":"bogus"}`)
	if rec.Code != 400 || rec.Body.String() != `{"detail":"Invalid lead status"}` {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	// validation precedes existence
	if rec := e.send("alice", "ws-alpha", "POST", "/api/lead/9999/status", `{"status":"bogus"}`); rec.Code != 400 {
		t.Errorf("unknown id + bad status: %d", rec.Code)
	}
}

func TestStatusNoteIgnored(t *testing.T) {
	e := newWriteEnv(t)
	before := leadRow(t, e, "ws-alpha", 14, "")
	rec := e.send("alice", "ws-alpha", "POST", "/api/lead/14/status", `{"status":"converted","note":"closed won"}`)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	row := leadRow(t, e, "ws-alpha", 14, "")
	if row["status"] != "converted" || row["notes"] != before["notes"] {
		t.Errorf("row %v", row)
	}
}

func TestDeleteLead(t *testing.T) {
	e := newWriteEnv(t)
	rec := e.send("alice", "ws-alpha", "DELETE", "/api/lead/13", "")
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("alice", "ws-alpha", "DELETE", "/api/lead/13", ""); rec.Code != 404 || rec.Body.String() != `{"detail":"Lead not found"}` {
		t.Errorf("again: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWriteRoutesViewerForbidden403(t *testing.T) {
	e := newWriteEnv(t)
	for _, c := range [][3]string{{"POST", "/api/lead", `{"company":"Nope"}`}, {"PUT", "/api/lead/10", `{"notes":"x"}`},
		{"POST", "/api/lead/14/status", `{"status":"new"}`}, {"DELETE", "/api/lead/12", ""}} {
		rec := e.send("bob", "ws-alpha", c[0], c[1], c[2])
		if rec.Code != 403 || rec.Body.String() != `{"detail":"Workspace permission denied: tables.write"}` {
			t.Errorf("%s %s: %d %s", c[0], c[1], rec.Code, rec.Body.String())
		}
		if rec := e.send("", "", c[0], c[1], c[2]); rec.Code != 401 {
			t.Errorf("%s %s no auth: %d", c[0], c[1], rec.Code)
		}
	}
}

func TestWriteRoutesCrossWorkspace404(t *testing.T) {
	e := newWriteEnv(t)
	before := leadRow(t, e, "ws-beta", 17, "")
	for _, c := range [][3]string{{"PUT", "/api/lead/17", `{"notes":"x"}`}, {"POST", "/api/lead/17/status", `{"status":"new"}`}, {"DELETE", "/api/lead/17", ""}} {
		rec := e.send("alice", "ws-alpha", c[0], c[1], c[2])
		if rec.Code != 404 || rec.Body.String() != `{"detail":"Lead not found"}` {
			t.Errorf("%s %s: %d %s", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	after := leadRow(t, e, "ws-beta", 17, "")
	if after == nil || after["updated_at"] != before["updated_at"] || after["notes"] != before["notes"] || after["status"] != before["status"] {
		t.Error("foreign lead was modified")
	}
}

func TestWritesAdvanceUpdatedAt(t *testing.T) {
	e := newWriteEnv(t)
	b1 := leadRow(t, e, "ws-alpha", 10, "")
	e.send("alice", "ws-alpha", "POST", "/api/lead/10/status", `{"status":"qualified"}`)
	b2 := leadRow(t, e, "ws-alpha", 10, "")
	e.send("alice", "ws-alpha", "PUT", "/api/lead/10", `{"notes":"z"}`)
	b3 := leadRow(t, e, "ws-alpha", 10, "")
	if b1["updated_at"] == b2["updated_at"] || b2["updated_at"] == b3["updated_at"] {
		t.Errorf("updated_at %v %v %v", b1["updated_at"], b2["updated_at"], b3["updated_at"])
	}
}

func writeActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case strings.HasSuffix(name, "_viewer"):
		return "bob", "ws-alpha"
	case strings.HasSuffix(name, "_nonmember"):
		return "alice", "ws-beta"
	case name == "create_other_workspace_same_key", name == "get_foreign_after_put":
		return "bob", "ws-beta"
	}
	return "alice", ""
}

func TestLeadWriteFixturesMatchPython(t *testing.T) {
	e := newWriteEnv(t)
	order := strings.Fields(`create_ok create_defaults create_nonascii create_collision create_collision_empty_city
create_collision_omitted_city create_other_workspace_same_key create_missing_company create_bad_type create_null_city
create_viewer create_nonmember create_no_auth get_created status_ok status_no_note status_invalid status_missing_lead
status_foreign_lead status_missing_field status_bad_id status_viewer get_after_status put_fields put_clear_field
put_empty_body put_unknown_field put_protected_fields put_missing put_foreign put_not_object put_viewer get_after_put
get_foreign_after_put delete_ok delete_again delete_foreign delete_bad_id delete_viewer get_after_delete`)
	createdID := ""
	for _, name := range order {
		fx := contract.Load(t, "leadwrites/"+name)
		who, ws := writeActor(name)
		path := fx.Request.Path
		if name == "get_created" {
			path = "/api/lead/" + createdID
		}
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		rec := e.send(who, ws, fx.Request.Method, path, body)
		got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
		for _, k := range []string{"Content-Type", "WWW-Authenticate"} {
			if v := rec.Header().Get(k); v != "" {
				got.Headers[k] = v
			}
		}
		if name == "create_ok" {
			createdID = strings.TrimSpace(strings.Split(strings.Split(rec.Body.String(), `"id":`)[1], "}")[0])
		}
		opts := []contract.Option{}
		if strings.HasPrefix(name, "create_") || name == "get_created" {
			opts = append(opts, contract.IgnorePaths("$.id"))
		}
		if strings.HasPrefix(name, "get_") {
			opts = append(opts, contract.IgnoreShapeOf("$.created_at", "$.updated_at"))
		}
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	}
}
