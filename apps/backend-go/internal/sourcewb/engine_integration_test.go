//go:build integration

package sourcewb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type scenario struct {
	Name        string                       `json:"name"`
	Kind        string                       `json:"kind"`
	Column      string                       `json:"column"`
	JobID       string                       `json:"job_id"`
	SubmitError string                       `json:"submit_error"`
	Handler     bool                         `json:"handler"`
	Payload     map[string]any               `json:"payload"`
	Flags       map[string]bool              `json:"flags"`
	People      map[string]json.RawMessage   `json:"people"`
	CRM         map[string]json.RawMessage   `json:"crm"`
	WorkspaceID string                       `json:"workspace_id"`
	WorkbookID  string                       `json:"workbook_id"`
	Seed        map[string][]json.RawMessage `json:"seed"`
	Result      json.RawMessage              `json:"result"`
	Events      []struct {
		Channel string          `json:"channel"`
		Message json.RawMessage `json:"message"`
	} `json:"events"`
	Emits []json.RawMessage          `json:"emits"`
	Calls []map[string]any           `json:"calls"`
	Gate  map[string][2]any          `json:"gate"`
	Fit   map[string]json.RawMessage `json:"fit"`
	State map[string]json.RawMessage `json:"state"`
}

var seedOrder = []string{"workbooks", "company_entities", "company_identifiers", "entity_blocking_keys", "workbook_rows",
	"collection_jobs", "collection_job_stages", "leads"}
var cleanOrder = []string{"jobs", "entity_review_pairs", "leads", "workbook_rows", "person_employments", "person_identifiers", "person_entities", "entity_blocking_keys", "company_identifiers", "company_entities",
	"collection_job_stages", "collection_jobs", "leads", "workbooks"}

const dumpSQL = `
SELECT json_build_object(
 'workbook', (SELECT json_build_object('status', status, 'total_rows', total_rows, 'source_config', source_config) FROM workbooks WHERE id = $1),
 'rows', (SELECT coalesce(json_agg(json_build_object('id', id, 'position', position, 'data', data, 'enrichments', enrichments,
          'lead_id', lead_id, 'canonical_entity_id', canonical_entity_id, 'corroboration_count', corroboration_count,
          'source_provider', source_provider, 'source_record_id', source_record_id, 'workspace_id', workspace_id) ORDER BY id), '[]')
          FROM workbook_rows WHERE workspace_id = $2),
 'company_entities', (SELECT coalesce(json_agg(to_jsonb(t) - 'first_seen' - 'last_seen' - 'accepted_fields' - 'record_version' - 'record_name_key' ORDER BY canonical_name, id), '[]') FROM company_entities t WHERE workspace_id = $2),
 'company_identifiers', (SELECT coalesce(json_agg(json_build_object('kind', kind, 'value', value, 'entity_id', entity_id) ORDER BY kind, value), '[]') FROM company_identifiers WHERE workspace_id = $2),
 'blocking_keys', (SELECT coalesce(json_agg(json_build_object('key', key, 'entity_id', entity_id) ORDER BY key, entity_id), '[]') FROM entity_blocking_keys WHERE workspace_id = $2),
 'review_pairs', (SELECT coalesce(json_agg(json_build_object('entity_id', entity_id, 'candidate', candidate, 'score', score, 'reason', reason, 'status', status) ORDER BY id), '[]') FROM entity_review_pairs WHERE workspace_id = $2),
 'person_entities', (SELECT coalesce(json_agg(to_jsonb(t) - 'first_seen' - 'last_seen' - 'accepted_fields' - 'record_version' - 'record_name_key' ORDER BY full_name, id), '[]') FROM person_entities t WHERE workspace_id = $2),
 'person_identifiers', (SELECT coalesce(json_agg(json_build_object('kind', kind, 'value', value, 'person_id', person_id) ORDER BY kind, value), '[]') FROM person_identifiers WHERE workspace_id = $2),
 'person_employments', (SELECT coalesce(json_agg(to_jsonb(t) - 'id' ORDER BY person_id, company_key), '[]') FROM person_employments t WHERE workspace_id = $2),
 'jobs', (SELECT coalesce(json_agg(json_build_object('job_type', type, 'payload', payload, 'executor', executor, 'status', status) ORDER BY id), '[]') FROM jobs WHERE workspace_id = $2)
)::text`

// ── fakes ──

type fakeCollector struct {
	sc    scenario
	calls []map[string]any
	wss   []string // the workspace each call was made for
}

func (f *fakeCollector) Submit(_ context.Context, ws, query string) (string, error) {
	f.calls = append(f.calls, map[string]any{"op": "submit", "query": query, "workspace_id": ws})
	if f.sc.SubmitError != "" {
		return "", errors.New(f.sc.SubmitError)
	}
	return f.sc.JobID, nil
}

type fakeGate struct{ sc scenario }

func leadKey(lead *wbcrud.Obj) string { return fmt.Sprint(get(lead, "id")) }

func (g fakeGate) ValidateLight(lead *wbcrud.Obj) (bool, string) {
	r, ok := g.sc.Gate[leadKey(lead)]
	if !ok {
		return true, ""
	}
	b, _ := r[0].(bool)
	s, _ := r[1].(string)
	return b, s
}

func (g fakeGate) AccountFit(lead, _ *wbcrud.Obj) (*wbcrud.Obj, error) {
	raw, ok := g.sc.Fit[leadKey(lead)]
	if !ok {
		return nil, fmt.Errorf("no recorded fit for lead %s", leadKey(lead))
	}
	return wbcrud.ParseObject(raw)
}

type fakePeople struct {
	sc    scenario
	calls []map[string]any
	wss   []string // the workspace each call was made for
}

func (f *fakePeople) FindPeopleByTitles(_ context.Context, q PeopleQuery) (PeopleResult, error) {
	titles := make([]any, len(q.Titles))
	for i, t := range q.Titles {
		titles[i] = t
	}
	f.wss = append(f.wss, q.Workspace)
	f.calls = append(f.calls, map[string]any{"op": "find_people_by_titles", "company": q.Company, "titles": titles,
		"geo": q.Geo, "seniority": q.Seniority, "max_people": float64(q.MaxPeople), "max_searches": float64(q.MaxSearches)})
	var s struct {
		People   []map[string]string `json:"people"`
		Used     int                 `json:"used"`
		Failures int                 `json:"failures"`
		Raise    *string             `json:"raise"`
	}
	raw, ok := f.sc.People[q.Company]
	if !ok {
		return PeopleResult{SearchesUsed: 1}, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return PeopleResult{}, err
	}
	if s.Raise != nil {
		return PeopleResult{}, errors.New(*s.Raise)
	}
	out := PeopleResult{SearchesUsed: s.Used, SearchFailures: s.Failures}
	for _, p := range s.People {
		out.People = append(out.People, Person{Name: p["name"], Title: p["title"], LinkedIn: p["linkedin"], EvidenceURL: p["evidence_url"]})
	}
	return out, nil
}

type fakeCRM struct {
	sc    scenario
	calls []map[string]any
}

func (f *fakeCRM) FetchContacts(_ context.Context, ws, name string, limit int, filter string, fm crm.FieldMap) crm.FetchResult {
	call := map[string]any{"op": name + ".fetch_contacts", "limit": float64(limit), "workspace_id": ws}
	fk := "list_id"
	if name == "salesforce" {
		fk = "where"
	}
	if filter != "" {
		call[fk] = filter
	} else {
		call[fk] = nil
	}
	if fm != nil {
		m := map[string]any{}
		for _, p := range fm {
			m[p.Key] = p.Value
		}
		call["field_map"] = m
	} else {
		call["field_map"] = nil
	}
	f.calls = append(f.calls, call)
	raw, ok := f.sc.CRM[name]
	if !ok {
		return crm.FetchResult{Error: "unscripted"}
	}
	var s struct {
		Success  bool             `json:"success"`
		Contacts []map[string]any `json:"contacts"`
		Error    string           `json:"error"`
		Raise    *string          `json:"raise"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&s); err != nil {
		panic(err)
	}
	if s.Raise != nil {
		return crm.FetchResult{Error: *s.Raise}
	}
	return crm.FetchResult{Success: s.Success, Contacts: s.Contacts, Error: s.Error}
}

type capPub struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (c *capPub) Publish(_ context.Context, wb string, msg map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg["__channel"] = "workbook:" + wb
	c.msgs = append(c.msgs, msg)
}

type capHook struct {
	automationhook.Hook
	emits []map[string]any
}

func (h *capHook) RowsAdded(_ context.Context, _ pgx.Tx, ws, wb string, ids []int64) (int, error) {
	h.emits = append(h.emits, map[string]any{"workspace_id": ws, "workbook_id": wb, "row_ids": ids})
	return 0, nil
}

// ── relabel (same rules as the recorder) ──

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var tsKeys = map[string]bool{"observed_at": true, "first_observed_at": true, "last_observed_at": true}

type labeler struct {
	rows map[float64]string
	ent  map[string]string
}

func newLabeler(state map[string]any, seedRows []json.RawMessage) labeler {
	seeded := map[float64]bool{}
	for _, r := range seedRows {
		var m map[string]any
		_ = json.Unmarshal(r, &m)
		seeded[m["id"].(float64)] = true
	}
	l := labeler{rows: map[float64]string{}, ent: map[string]string{}}
	n := 0
	rows, _ := state["rows"].([]any)
	var order []string
	for _, x := range rows {
		r := x.(map[string]any)
		id := r["id"].(float64)
		if seeded[id] {
			l.rows[id] = fmt.Sprintf("S%d", int(id))
		} else {
			n++
			l.rows[id] = fmt.Sprintf("R%d", n)
		}
		data, _ := r["data"].(map[string]any)
		for _, v := range []any{r["canonical_entity_id"], data["canonical_person_id"], data["account_id"]} {
			if s, ok := v.(string); ok && uuidRE.MatchString(s) {
				order = append(order, s)
			}
		}
	}
	persons := map[string]bool{}
	for _, k := range []string{"company_entities", "person_entities"} {
		list, _ := state[k].([]any)
		for _, x := range list {
			id := x.(map[string]any)["id"].(string)
			order = append(order, id)
			if k == "person_entities" {
				persons[id] = true
			}
		}
	}
	cnt := map[string]int{}
	for _, v := range order {
		if _, done := l.ent[v]; done || !uuidRE.MatchString(v) {
			continue
		}
		k := "E"
		if persons[v] {
			k = "P"
		}
		cnt[k]++
		l.ent[v] = fmt.Sprintf("%s%d", k, cnt[k])
	}
	return l
}

func (l labeler) walk(x any, key string) any {
	switch v := x.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range v {
			out[k] = l.walk(e, k)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = l.walk(e, key)
		}
		return out
	case string:
		if tsKeys[key] {
			return "<ts>"
		}
		if s, ok := l.ent[v]; ok {
			return s
		}
		return v
	}
	return x
}

func (l labeler) rid(v any) any {
	if f, ok := v.(float64); ok {
		if s, ok := l.rows[f]; ok {
			return s
		}
	}
	return v
}

func generic(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func rawGeneric(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var out any
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// canonList: nil and empty lists are equal; state lists are compared as multisets
// (the dump orders some tables by random ids).
func canonList(v any, sorted bool) any {
	if v == nil {
		return []any{}
	}
	l, ok := v.([]any)
	if !ok {
		return v
	}
	if len(l) == 0 {
		return []any{}
	}
	if sorted {
		l = append([]any(nil), l...)
		sort.Slice(l, func(i, j int) bool {
			a, _ := json.Marshal(l[i])
			b, _ := json.Marshal(l[j])
			return string(a) < string(b)
		})
	}
	return l
}

func diff(t *testing.T, name, what string, got, want any) {
	t.Helper()
	if _, isList := want.([]any); isList || want == nil {
		if _, gl := got.([]any); gl || got == nil {
			sorted := strings.HasPrefix(what, "state.")
			got, want = canonList(got, sorted), canonList(want, sorted)
		}
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("%s: %s differs\n got %s\nwant %s", name, what, g, w)
	}
}

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestSourceScenarioVectors(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	owner := ownerPool(t)
	ctx := context.Background()
	_, raws := loadVectors(t)
	for _, raw := range raws {
		var sc scenario
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatal(err)
		}
		t.Run(sc.Name, func(t *testing.T) {
			ws := sc.WorkspaceID
			for _, tb := range cleanOrder {
				if _, err := owner.Exec(ctx, "DELETE FROM "+tb+" WHERE workspace_id = $1", ws); err != nil {
					t.Fatal(err)
				}
			}
			for _, tb := range seedOrder {
				testpg.FreeRecordIDs(t, tb, sc.Seed[tb]...) // recorded serial ids sit below testpg.SeqFloor
				for _, r := range sc.Seed[tb] {
					if tb == "leads" {
						// Replay the historical collection snapshot verbatim. The
						// shared-record Lead adapter has its own integration coverage;
						// running it during fixture load changes the graph under test.
						tx, err := owner.Begin(ctx)
						if err != nil {
							t.Fatal(err)
						}
						if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err == nil {
							_, err = tx.Exec(ctx, "INSERT INTO leads SELECT * FROM json_populate_record(NULL::leads,$1::json)", string(r))
						}
						if err != nil {
							_ = tx.Rollback(ctx)
							t.Fatal(err)
						}
						if err = tx.Commit(ctx); err != nil {
							t.Fatal(err)
						}
						continue
					}
					if _, err := owner.Exec(ctx, "INSERT INTO "+tb+" SELECT * FROM json_populate_record(NULL::"+tb+", $1::json)", string(r)); err != nil {
						t.Fatalf("seed %s: %v", tb, err)
					}
				}
			}
			col := &fakeCollector{sc: sc}
			ppl := &fakePeople{sc: sc}
			cr := &fakeCRM{sc: sc}
			pub := &capPub{}
			hook := &capHook{}
			d := Deps{Pool: pool, Collector: col, Gate: fakeGate{sc}, People: ppl, CRM: cr, Hook: hook, Progress: pub,
				CRMImportEnabled: sc.Flags["CRM_IMPORT_ENABLED"], PeopleSearchEnabled: sc.Flags["PEOPLE_SEARCH_SOURCE_ENABLED"]}
			var result any
			if sc.Handler {
				p := map[string]any{"workbook_id": sc.WorkbookID, "column_id": sc.Column, "workspace_id": ws}
				for k, v := range sc.Payload {
					p[k] = v
				}
				b, _ := json.Marshal(p)
				if err := Handle(ctx, d, &queue.Claim{ID: 1, Type: JobType, WorkspaceID: ws, Payload: b}); err != nil {
					t.Fatal(err)
				}
			} else {
				res, err := Materialize(ctx, d, ws, sc.WorkbookID, sc.Column)
				if err != nil {
					t.Fatal(err)
				}
				result = generic(t, res)
			}

			for _, got := range ppl.wss { // the shared search cache is salted per workspace
				if got != ws {
					t.Fatalf("people search ran for workspace %q, want %q", got, ws)
				}
			}
			var dump string
			if err := owner.QueryRow(ctx, dumpSQL, "wb-sourcewb", ws).Scan(&dump); err != nil {
				t.Fatal(err)
			}
			var state map[string]any
			if err := json.Unmarshal([]byte(dump), &state); err != nil {
				t.Fatal(err)
			}
			l := newLabeler(state, sc.Seed["workbook_rows"])
			for _, x := range state["rows"].([]any) {
				r := x.(map[string]any)
				r["id"] = l.rid(r["id"])
			}
			if m, ok := result.(map[string]any); ok {
				if ids, ok := m["added_row_ids"].([]any); ok {
					for i := range ids {
						ids[i] = l.rid(ids[i])
					}
				}
			}
			var events []any
			for _, m := range pub.msgs {
				ch := m["__channel"]
				delete(m, "__channel")
				g := generic(t, m).(map[string]any)
				if v, ok := g["rowId"]; ok {
					g["rowId"] = l.rid(v)
				}
				events = append(events, map[string]any{"channel": ch, "message": g})
			}
			var emits []any
			for _, e := range hook.emits {
				g := generic(t, e).(map[string]any)
				ids := g["row_ids"].([]any)
				for i := range ids {
					ids[i] = l.rid(ids[i])
				}
				emits = append(emits, g)
			}

			diff(t, sc.Name, "result", l.walk(result, ""), rawGeneric(t, sc.Result))
			var wantEvents []any
			for _, e := range sc.Events {
				wantEvents = append(wantEvents, map[string]any{"channel": e.Channel, "message": rawGeneric(t, e.Message)})
			}
			diff(t, sc.Name, "events", l.walk(events, ""), wantEvents)
			var wantEmits []any
			for _, e := range sc.Emits {
				wantEmits = append(wantEmits, rawGeneric(t, e))
			}
			diff(t, sc.Name, "emits", emits, wantEmits)
			for k, want := range sc.State {
				w := rawGeneric(t, want)
				if sharedWarehouseState(t, sc, k, state, l) {
					continue
				}
				if k == "jobs" { // Python's recorded executor; every job is go since the plan 15 cutover
					if js, ok := w.([]any); ok {
						for _, j := range js {
							if m, ok := j.(map[string]any); ok && m["executor"] == "python" {
								m["executor"] = "go"
							}
						}
					}
				}
				diff(t, sc.Name, "state."+k, l.walk(state[k], k), w)
			}

			// calls the fakes saw (Python's provider construction is not a call here)
			var gotCalls []any
			for _, c := range append(append(col.calls, ppl.calls...), cr.calls...) {
				gotCalls = append(gotCalls, generic(t, c))
			}
			var wantCalls []any
			for _, c := range sc.Calls {
				if c["op"] == "crosslinked_init" {
					continue
				}
				wantCalls = append(wantCalls, generic(t, c))
			}
			sortCalls(gotCalls)
			sortCalls(wantCalls)
			diff(t, sc.Name, "calls", gotCalls, wantCalls)
		})
	}
}

// People/CRM sources now populate the shared warehouse, where the Python
// recording had no companies (and no CRM people). Assert those new facts and
// links against the recorded source rows; retain exact replay for row contents,
// events, provider calls, and the historical person/employment observations.
func sharedWarehouseState(t *testing.T, sc scenario, key string, state map[string]any, l labeler) bool {
	t.Helper()
	if sc.Name != "people_basic" && sc.Name != "people_from_column" && sc.Name != "people_budget" && sc.Name != "crm_hubspot" && sc.Name != "crm_salesforce_inline" {
		return false
	}
	if key != "company_entities" && key != "company_identifiers" && key != "person_entities" && key != "person_identifiers" && key != "person_employments" {
		return false
	}
	wantCompanies, wantPeople := map[string]bool{}, map[string]bool{}
	rows, _ := rawGeneric(t, sc.State["rows"]).([]any)
	for _, item := range rows {
		r, _ := item.(map[string]any)
		data, _ := r["data"].(map[string]any)
		if name, ok := data["company"].(string); ok && name != "" {
			wantCompanies[strings.ToLower(strings.TrimSpace(name))] = true
		}
		for _, field := range []string{"contact_person", "full_name"} {
			if name, ok := data[field].(string); ok && name != "" {
				wantPeople[name] = true
			}
		}
	}
	companies, people := map[string]string{}, map[string]string{}
	companyNames, personNames := map[string]bool{}, map[string]bool{}
	for _, item := range state["company_entities"].([]any) {
		r := item.(map[string]any)
		name := r["canonical_name"].(string)
		companies[r["id"].(string)] = name
		companyNames[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, item := range state["person_entities"].([]any) {
		r := item.(map[string]any)
		name := r["full_name"].(string)
		people[r["id"].(string)] = name
		personNames[name] = true
		if id, ok := r["company_entity_id"].(string); ok && companies[id] == "" {
			t.Fatalf("%s: person links a missing company", sc.Name)
		}
	}
	switch key {
	case "company_entities":
		if !reflect.DeepEqual(companyNames, wantCompanies) || len(companies) != len(wantCompanies) {
			t.Fatalf("%s: company facts got %v want recorded %v", sc.Name, companyNames, wantCompanies)
		}
	case "company_identifiers", "person_identifiers":
		for _, item := range state[key].([]any) {
			r := item.(map[string]any)
			if key == "company_identifiers" && companies[r["entity_id"].(string)] == "" {
				t.Fatal("identifier links a missing company")
			}
			if key == "person_identifiers" && people[r["person_id"].(string)] == "" {
				t.Fatal("identifier links a missing person")
			}
		}
		if key == "person_identifiers" && strings.HasPrefix(sc.Name, "people_") {
			diff(t, sc.Name, "state."+key, l.walk(state[key], key), rawGeneric(t, sc.State[key]))
		}
	case "person_entities":
		want := rawGeneric(t, sc.State[key]).([]any)
		if len(want) == 0 {
			if !reflect.DeepEqual(personNames, wantPeople) || len(people) != len(wantPeople) {
				t.Fatalf("%s: people got %v want recorded %v", sc.Name, personNames, wantPeople)
			}
		} else {
			got := l.walk(state[key], key).([]any)
			for _, list := range [][]any{got, want} {
				for _, item := range list {
					delete(item.(map[string]any), "company_entity_id")
				}
			}
			diff(t, sc.Name, "state.historical people", got, want)
		}
	case "person_employments":
		for _, item := range state[key].([]any) {
			r := item.(map[string]any)
			if people[r["person_id"].(string)] == "" {
				t.Fatal("employment links a missing person")
			}
			if id, ok := r["company_entity_id"].(string); ok && companies[id] != r["company_name"] {
				t.Fatalf("employment links wrong company: %v", r)
			}
		}
		want := rawGeneric(t, sc.State[key]).([]any)
		if len(want) > 0 {
			got := l.walk(state[key], key).([]any)
			for _, list := range [][]any{got, want} {
				for _, item := range list {
					r := item.(map[string]any)
					delete(r, "company_entity_id")
					delete(r, "accepted_fields")
					delete(r, "ended_at")
				}
			}
			diff(t, sc.Name, "state.historical employments", got, want)
		} else if len(state[key].([]any)) != len(wantPeople) {
			t.Fatalf("%s: CRM employment count differs from recorded people", sc.Name)
		}
	}
	return true
}

func sortCalls(c []any) {
	sort.SliceStable(c, func(i, j int) bool {
		return strings.Compare(fmt.Sprint(c[i].(map[string]any)["op"]), fmt.Sprint(c[j].(map[string]any)["op"])) < 0
	})
}
