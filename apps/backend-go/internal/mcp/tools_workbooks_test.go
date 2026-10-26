package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type wbVector struct {
	Description string          `json:"description"`
	Columns     json.RawMessage `json:"columns"`
}

func TestPlanColumnsVectorsMatchPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan_columns.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []wbVector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vs {
		got, err := json.Marshal(planColumns(v.Description))
		if err != nil {
			t.Fatal(err)
		}
		var want, have any
		_ = json.Unmarshal(v.Columns, &want)
		_ = json.Unmarshal(got, &have)
		wb, _ := json.Marshal(want)
		hb, _ := json.Marshal(have)
		if string(wb) != string(hb) {
			t.Fatalf("%q: got %s want %s", v.Description, got, v.Columns)
		}
		// key order is wire parity: id, name, type, lead_field
		if !strings.HasPrefix(string(got), `[{"id":"company","name":"Company","type":"lead_field","lead_field":"company"}`) {
			t.Fatalf("%q: key order %s", v.Description, got)
		}
	}
}

func colIDs(cols []Column) []string {
	var ids []string
	for _, c := range cols {
		ids = append(ids, c.Get("id").(string))
	}
	return ids
}

func TestPlanColumnsUnicodeAndEmpty(t *testing.T) {
	cases := []struct {
		desc string
		want string
	}{
		{"", "company,email,contact_person"},
		{"CTO of 日本 firms", "company,contact_person,contact_title,email"},
		{"CALL the VP in LOCATION", "company,phone,contact_person,contact_title,city,email"},
		// Python lowers U+0130 to "i"+U+0307, so the keyword does not match.
		{"t\u0130tle", "company,email,contact_person"},
		// Kelvin sign lowers to ASCII k in both languages.
		{"\u212Aeep linkedin", "company,linkedin_url,email,contact_person"},
		{"email email phone phone", "company,email,phone,contact_person"},
	}
	for _, c := range cases {
		if got := strings.Join(colIDs(planColumns(c.desc)), ","); got != c.want {
			t.Fatalf("%q: got %s want %s", c.desc, got, c.want)
		}
	}
}

// fixedWB answers a recorded workbook id and remembers what it was given.
type fixedWB struct {
	id          string
	name, desc  string
	cols        []Column
	ws          string
	found       bool
	deleted     string
	createCalls int
}

func (f *fixedWB) Create(_ context.Context, _ pgx.Tx, ws, name, desc string, cols []Column) (string, error) {
	f.createCalls++
	f.ws, f.name, f.desc, f.cols = ws, name, desc, cols
	return f.id, nil
}
func (f *fixedWB) Delete(_ context.Context, _ pgx.Tx, ws, id string) (bool, error) {
	f.ws, f.deleted = ws, id
	return f.found, nil
}

func wbToolByName(t *testing.T, d Deps, name string) WriteTool {
	t.Helper()
	for _, w := range workbookWriteTools(d) {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("no tool %s", name)
	return WriteTool{}
}

func fixtureRequest(t *testing.T, file string) (string, Args) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/mcp/tools/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Request struct {
			Params struct {
				Name      string `json:"name"`
				Arguments Args   `json:"arguments"`
			} `json:"params"`
		} `json:"request"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Request.Params.Name, f.Request.Params.Arguments
}

func runWB(t *testing.T, w WriteTool, c Ctx, args Args) string {
	t.Helper()
	out, err := w.Apply(context.Background(), nil, c, args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return out
}

var wbCtx = Ctx{WorkspaceID: "ws-alpha", Capabilities: AllCaps}

func TestCreateWorkbookMatchesFixture(t *testing.T) {
	for _, file := range []string{
		"writes__create_workbook.json", "writes__create_workbook_minimal.json",
		"writes__create_workbook_named.json", "writes__create_workbook_unicode.json",
		"writes__create_workbook_long_description_name_truncated.json",
		"writes__create_workbook_idempotent_replay.json",
	} {
		_, args := fixtureRequest(t, file)
		want := fixtureText(t, file)
		var parsed struct {
			ID string `json:"workbook_id"`
		}
		_ = json.Unmarshal([]byte(want), &parsed)
		f := &fixedWB{id: parsed.ID}
		got := runWB(t, wbToolByName(t, Deps{Workbooks: f}, "create_workbook"), wbCtx, args)
		if got != want {
			t.Fatalf("%s:\n got %s\nwant %s", file, got, want)
		}
		if f.ws != "ws-alpha" {
			t.Fatalf("%s: workspace %q", file, f.ws)
		}
	}
}

func TestCreateWorkbookDefaultNameTruncatesByCodePoint(t *testing.T) {
	desc := strings.Repeat("日本", 30) + "\U0001F600tail"
	f := &fixedWB{id: "x"}
	runWB(t, wbToolByName(t, Deps{Workbooks: f}, "create_workbook"), wbCtx, Args{"description": desc})
	want := "MCP \u2014 " + string([]rune(desc)[:40])
	if f.name != want || utf8.RuneCountInString(f.name) != 46 {
		t.Fatalf("name %q", f.name)
	}
	if f.desc != desc {
		t.Fatal("description must be stored whole")
	}
	// An emoji straddling the cut is never split.
	f2 := &fixedWB{id: "y"}
	d2 := strings.Repeat("a", 39) + "\U0001F600zz"
	runWB(t, wbToolByName(t, Deps{Workbooks: f2}, "create_workbook"), wbCtx, Args{"description": d2})
	if f2.name != "MCP \u2014 "+strings.Repeat("a", 39)+"\U0001F600" {
		t.Fatalf("name %q", f2.name)
	}
	// empty name and null name fall back to the default (Python `or`).
	for _, n := range []any{"", nil} {
		f3 := &fixedWB{id: "z"}
		runWB(t, wbToolByName(t, Deps{Workbooks: f3}, "create_workbook"), wbCtx, Args{"description": "abc", "name": n})
		if f3.name != "MCP \u2014 abc" {
			t.Fatalf("name %q", f3.name)
		}
	}
}

func TestCreateWorkbookMissingDescription(t *testing.T) {
	f := &fixedWB{id: "x"}
	w := wbToolByName(t, Deps{Workbooks: f}, "create_workbook")
	for _, a := range []Args{{}, {"description": nil}} {
		if got := runWB(t, w, wbCtx, a); got != `{"error": "description is required"}` {
			t.Fatalf("got %s", got)
		}
	}
	if f.createCalls != 0 {
		t.Fatal("port must not be called")
	}
	for _, a := range []Args{{"description": 5.0}, {"description": "x", "name": 7.0}} {
		if _, err := w.Apply(context.Background(), nil, wbCtx, a); err == nil {
			t.Fatalf("%v: want ArgError", a)
		}
	}
}

func TestDeleteWorkbookArguments(t *testing.T) {
	f := &fixedWB{found: true}
	w := wbToolByName(t, Deps{Workbooks: f}, "delete_workbook")
	_, noArgs := fixtureRequest(t, "writes__delete_workbook_missing_id.json")
	if got := runWB(t, w, wbCtx, noArgs); got != fixtureText(t, "writes__delete_workbook_missing_id.json") {
		t.Fatalf("got %s", got)
	}
	if got := runWB(t, w, wbCtx, Args{"workbook_id": ""}); got != `{"error": "workbook_id is required"}` {
		t.Fatalf("got %s", got)
	}
	if got := runWB(t, w, wbCtx, Args{"workbook_id": strings.Repeat("é", 65)}); got != `{"error": "workbook_id must be at most 64 characters"}` {
		t.Fatalf("got %s", got)
	}
	if _, err := w.Apply(context.Background(), nil, wbCtx, Args{"workbook_id": 12.0}); err == nil {
		t.Fatal("want ArgError for non-string")
	}
	if f.deleted != "" {
		t.Fatal("port must not be called on bad arguments")
	}
	id64 := strings.Repeat("é", 64)
	if got := runWB(t, w, wbCtx, Args{"workbook_id": id64}); !strings.Contains(got, `"deleted"`) {
		t.Fatalf("64 chars must be accepted: %s", got)
	}
	f.found = false
	if got := runWB(t, w, wbCtx, Args{"workbook_id": "gone"}); got != `{"status": "not_found", "workbook_id": "gone"}` {
		t.Fatalf("got %s", got)
	}
}

func TestWorkbookToolsRequireCapabilityAndRole(t *testing.T) {
	ts := workbookWriteTools(Deps{})
	if len(ts) != 2 {
		t.Fatalf("%d tools", len(ts))
	}
	for _, w := range ts {
		switch w.Name {
		case "create_workbook":
			if w.Capability != "workbooks:write" || strings.Join(w.Roles, ",") != "admin,member,editor" {
				t.Fatalf("%+v", w)
			}
		case "delete_workbook":
			if w.Capability != "workbooks:delete" || strings.Join(w.Roles, ",") != "admin" {
				t.Fatalf("%+v", w)
			}
		default:
			t.Fatal(w.Name)
		}
		if w.Gate != nil {
			t.Fatalf("%s: unexpected gate", w.Name)
		}
	}
}
