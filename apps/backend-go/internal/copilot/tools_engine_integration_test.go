//go:build integration

package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilotauth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourceingest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// normRules are record_native_tools.py norm() rules.
var normRules = []struct {
	rx  *regexp.Regexp
	rep string
}{
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "$$UUID"},
	{regexp.MustCompile(`\b[0-9a-f]{32}\b`), "$$HEX"},
	{regexp.MustCompile(`\b(src|agent)_[0-9a-f]{8}\b`), "${1}_$$8"},
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(\+00:00|Z)?`), "$$TS"},
	{regexp.MustCompile(`"(queue_job_id|source_job_id|job_id)": \d+`), `"${1}": "$$ID"`},
	{regexp.MustCompile(`("refresh_workbook:[^"]*:policy:)\d+`), "${1}$$EPOCH"},
}

func normStr(s string) string {
	for _, r := range normRules {
		s = r.rx.ReplaceAllString(s, r.rep)
	}
	return s
}

type abCall struct {
	Fn string         `json:"fn"`
	KW map[string]any `json:"kw"`
}

type fakeAB struct{ calls []abCall }

func strsOrNil(s []string) any {
	if s == nil {
		return nil
	}
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (f *fakeAB) SearchCompanies(_ context.Context, p ambitionbox.SearchParams) (*ambitionbox.Obj, error) {
	f.calls = append(f.calls, abCall{"search_companies", map[string]any{"page": float64(p.Page), "limit": float64(p.Limit),
		"sort_by": p.SortBy, "industry": strsOrNil(p.Industry), "rating": strOrNil(p.Rating)}})
	if p.Rating == "boom" {
		return nil, &ambitionbox.UpstreamError{Status: 503, Msg: "AmbitionBox API 503: upstream down"}
	}
	co := wbcrud.NewObj()
	co.Set("id", 1)
	co.Set("name", "Co 1")
	o := wbcrud.NewObj()
	o.Set("companies", []any{co})
	o.Set("total", 1)
	o.Set("page", p.Page)
	o.Set("source_total", 9)
	o.Set("total_pages", 1)
	o.Set("has_more", false)
	return o, nil
}

func (f *fakeAB) CollectCompaniesWith(_ context.Context, p ambitionbox.CollectParams) (ambitionbox.Collection, error) {
	f.calls = append(f.calls, abCall{"collect_companies", map[string]any{"requested_count": float64(p.Requested),
		"max_pages": float64(p.MaxPages), "industry": strsOrNil(p.Industry), "sort_by": p.SortBy, "rating": strOrNil(p.Rating)}})
	var recs []sourceingest.Record
	for i := 0; i < min(3, p.Requested); i++ {
		d := wbcrud.NewObj()
		d.Set("id", i)
		d.Set("name", fmt.Sprintf("Co %d", i))
		recs = append(recs, sourceingest.Record{Data: d})
	}
	return ambitionbox.Collection{Provider: "ambitionbox", Records: recs, RequestedCount: p.Requested, TargetMet: true}, nil
}

func (f *fakeAB) GetCompanyJobs(_ context.Context, id, page int) (*ambitionbox.Obj, error) {
	f.calls = append(f.calls, abCall{"get_company_jobs", map[string]any{"company_id": float64(id), "page": float64(page)}})
	o := wbcrud.NewObj()
	o.Set("jobs", []any{})
	o.Set("total", 0)
	o.Set("page", page)
	o.Set("total_pages", 0)
	return o, nil
}

// semantic decodes JSON-looking strings so stored JSON text compares by value (Go stores some columns
// compactly or with sorted keys where SQLAlchemy kept insertion order).
func semantic(v any) any {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "{") || strings.HasPrefix(x, "[") {
			var d any
			if json.Unmarshal([]byte(x), &d) == nil {
				return semantic(d)
			}
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = semantic(e)
		}
	case []any:
		for i, e := range x {
			x[i] = semantic(e)
		}
	}
	return v
}

func toolSnapshot(t *testing.T, owner *pgxpool.Pool, ws string, before int64) map[string]any {
	t.Helper()
	ctx := context.Background()
	q := func(sql string, args ...any) []any {
		rows, err := owner.Query(ctx, `SELECT coalesce(json_agg(r), '[]')::text FROM (`+sql+`) r`, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var s string
		for rows.Next() {
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
		}
		var out []any
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	snap := map[string]any{
		"jobs": q(`SELECT type, workspace_id, payload::text AS payload, fire_key, executor, status, priority, max_retries,
			next_run_at IS NOT NULL AND next_run_at > now() AT TIME ZONE 'utc' + interval '1 minute' AS delayed
			FROM jobs WHERE id > $1 AND (workspace_id = $2 OR payload::text LIKE $3) ORDER BY id`, before, ws, "%"+ws+"%"),
		"workbooks": q(`SELECT name, description, status, source_type, action_idempotency_key, source_config::text AS source_config,
			filter_criteria::text AS filter_criteria, columns_config::text AS columns_config, total_rows, completed_rows,
			sync_to_leads, budget_max_usd, budget_spent_usd, refresh_policy::text AS refresh_policy
			FROM workbooks WHERE workspace_id = $1 ORDER BY name, id`, ws),
		"collection_jobs": q(`SELECT query, intent, intent_details, status FROM collection_jobs WHERE workspace_id = $1 ORDER BY created_at, id`, ws),
	}
	b, _ := json.Marshal(snap)
	var out map[string]any
	_ = json.Unmarshal([]byte(deepNormJSON(t, b)), &out)
	return semantic(out).(map[string]any)
}

// deepNormJSON applies normStr to every string value.
func deepNormJSON(t *testing.T, b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return normStr(x)
		case map[string]any:
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		}
		return v
	}
	out, _ := json.Marshal(walk(v))
	return string(out)
}

// goRefreshJob is the shape Go's refresh.SetPolicy enqueues (the refresh package's documented deviation:
// workspace_id is stamped on the row and a policy fire key dedupes the chain start).
func goRefreshJob(j map[string]any) {
	if j["type"] == "refresh_workbook" {
		delete(j, "workspace_id")
		delete(j, "fire_key")
		delete(j, "executor")
		if p, ok := j["payload"].(map[string]any); ok {
			for k := range p {
				if k != "workbook_id" && k != "workspace_id" {
					delete(p, k)
				}
			}
		}
	}
}

func TestEngineToolsMatchPython(t *testing.T) {
	var f struct {
		WS    string `json:"workspace_id"`
		Slug  string `json:"slug"`
		Base  int64  `json:"lead_id_base"`
		Leads [][]any
		Cases []struct {
			Name   string          `json:"name"`
			Args   json.RawMessage `json:"args"`
			Result string          `json:"result"`
			AB     []abCall        `json:"ambitionbox_calls"`
			After  map[string]any  `json:"after"`
		} `json:"cases"`
	}
	readVectors(t, "native_tools.json", &f)
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	ws := f.WS
	exec := func(sql string, args ...any) {
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	clean := func() {
		for _, tb := range []string{"workbook_rows", "workbooks", "collection_job_stages", "collection_jobs", "jobs", "leads"} {
			exec(`DELETE FROM `+tb+` WHERE workspace_id = $1`, ws)
		}
		exec(`DELETE FROM jobs WHERE payload::text LIKE $1`, "%"+ws+"%")
		exec(`DELETE FROM workspaces WHERE id = $1`, ws)
	}
	clean()
	t.Cleanup(clean)
	exec(`INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar, $1::text, $2::text)`, ws, f.Slug)
	for _, l := range f.Leads {
		vals := []any{f.Base + int64(l[0].(float64)), ws}
		for _, v := range l[1:] {
			if n, ok := v.(float64); ok {
				vals = append(vals, int64(n))
			} else {
				vals = append(vals, v)
			}
		}
		exec(`INSERT INTO leads (id, workspace_id, company, city, specialization, email, phone, linkedin_url, contact_person,
			website, description, company_size, score, score_tier, status, decision_makers, founded_year, revenue_range)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, vals...)
	}
	seedWB := func(id, refresh string) {
		var rp any
		if refresh != "" {
			rp = refresh
		}
		exec(`INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config, filter_criteria,
			columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd, refresh_policy)
			VALUES ($1, 'Seed WB', '', 'draft', $2, 'empty', '{}', '{}',
			'[{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"}]', 0, 0, true, 0, 0, $3::json)`, id, ws, rp)
	}
	seedWB("wb-rec-1", "")
	seedWB("wb-rec-2", "")
	seedWB("wb-rec-3", `{"enabled": false, "interval": "daily"}`)

	ab := &fakeAB{}
	tn := &Turn{Deps: Deps{Pool: pool, Engine: &Engine{Ambition: ab, CollectionLedgerPG: true}}, WS: ws, Slug: f.Slug,
		Auth: copilotauth.Resolved{WorkspaceID: ws, Slug: f.Slug}}
	for i, c := range f.Cases {
		name := fmt.Sprintf("%02d_%s", i, c.Name)
		var before int64
		if err := owner.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM jobs`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		av, err := canonjson.Decode(c.Args)
		if err != nil {
			t.Fatal(err)
		}
		ab.calls = nil
		res, handled, err := tn.engineTool(ctx, parsedCall{Name: c.Name, Args: av.(map[string]any)})
		if err != nil || !handled {
			t.Fatalf("%s: handled=%v err=%v", name, handled, err)
		}
		got := normStr(res)
		if strings.HasPrefix(c.Result, `{"provider": "ambitionbox"`) { // the recorder's fake summary has fewer keys
			tail := c.Result[strings.Index(c.Result, `"companies"`):]
			if !strings.HasSuffix(got, tail) {
				t.Errorf("%s:\n got %s\nwant tail %s", name, got, tail)
			}
		} else if got != c.Result {
			t.Errorf("%s:\n got %s\nwant %s", name, got, c.Result)
		}
		var wantAB []abCall
		b, _ := json.Marshal(c.AB)
		_ = json.Unmarshal(b, &wantAB)
		if len(wantAB) != 0 || len(ab.calls) != 0 {
			if !reflect.DeepEqual(ab.calls, wantAB) {
				t.Errorf("%s: ambitionbox calls %v, want %v", name, ab.calls, wantAB)
			}
		}
		gotAfter := toolSnapshot(t, owner, ws, before)
		wantAfter := semantic(c.After).(map[string]any)
		for _, snap := range []map[string]any{gotAfter, wantAfter} {
			for _, j := range snap["jobs"].([]any) {
				goRefreshJob(j.(map[string]any))
			}
		}
		for _, k := range []string{"jobs", "workbooks", "collection_jobs"} {
			if !reflect.DeepEqual(gotAfter[k], wantAfter[k]) {
				gb, _ := json.Marshal(gotAfter[k])
				wb, _ := json.Marshal(wantAfter[k])
				t.Errorf("%s: %s\n got %s\nwant %s", name, k, gb, wb)
			}
		}
	}
}

func TestEngineToolRoleCheck(t *testing.T) {
	e := nativeSetup(t)
	ctx := context.Background()
	for uid, want := range map[int32]bool{2: true, 3: false, 5: false} { // editor, viewer, member
		tn := &Turn{Deps: Deps{Pool: e.pool, Engine: &Engine{}}, WS: e.ws, Slug: "s", UID: i32(uid)}
		res, handled, err := tn.engineTool(ctx, parsedCall{Name: "set_workbook_refresh", Args: map[string]any{"workbook_id": "none", "interval": "daily"}})
		if err != nil || !handled {
			t.Fatalf("uid %d: handled=%v err=%v", uid, handled, err)
		}
		if (res != errInsufficientRole) != want {
			t.Errorf("uid %d: %s", uid, res)
		}
	}
	// a read-only engine tool needs no role
	tn := &Turn{Deps: Deps{Pool: e.pool, Engine: &Engine{}}, WS: e.ws, Slug: "s", UID: i32(3)}
	if res, handled, err := tn.engineTool(ctx, parsedCall{Name: "compare_leads", Args: map[string]any{"lead_ids": []any{e.acme}}}); err != nil || !handled || !strings.Contains(res, `"Acme"`) {
		t.Fatalf("compare_leads as viewer: %s %v %v", res, handled, err)
	}
}
