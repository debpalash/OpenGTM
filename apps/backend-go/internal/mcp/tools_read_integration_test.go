//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// pgLeads is the read half of the production LeadPort over plan 05 code.
type pgLeads struct{ noLeads }

func (*pgLeads) List(ctx context.Context, tx pgx.Tx, p ListParams) ([]Lead, error) {
	return leadsread.ListLeads(ctx, tx, p)
}
func (*pgLeads) ByID(ctx context.Context, tx pgx.Tx, id int64) (*Lead, error) {
	ls, err := leadsread.LeadsByIDs(ctx, tx, []int64{id})
	if err != nil || len(ls) == 0 {
		return nil, err
	}
	return &ls[0], nil
}
func (*pgLeads) Stats(ctx context.Context, tx pgx.Tx) (StatsResult, error) {
	return leadsread.Stats(ctx, tx)
}

type seedLead struct {
	ws                                                     int // 0 alpha, 1 beta
	company, city, website, email, phone, person, spec, sz string
	status                                                 string
	score                                                  int
	tier                                                   string
	dm                                                     *string
}

var seedDM = `[{"name": "Sam Lee", "title": "CEO"}]`

// The mcp_seed.py leads in insertion order (fixture ids 1-8).
var seedLeads = []seedLead{
	{0, "Acme Robotics", "Berlin", "https://acme.example.com", "info@acme.example.com", "+49 30 555 0101", "Ada Lovelace", "Robotics", "51-200", "new", 80, "hot", &seedDM},
	{0, "Globex Corp", "Austin", "https://globex.example.com", "hello@globex.example.com", "+1 512 555 0102", "Hank Scorpio", "Energy", "201-500", "contacted", 65, "warm", nil},
	{0, "Éco Solutions", "Pune", "https://eco.example.com", "", "", "Isha Rao", "Sustainability", "11-50", "new", 40, "cold", nil},
	{0, "Suppressed Inc", "Oslo", "https://sup.example.com", "stop@sup.example.com", "", "", "Retail", "1-10", "new", 30, "cold", nil},
	{0, "Delete Me Ltd", "Lyon", "", "bye@delete.example.com", "", "", "", "", "new", 10, "unqualified", nil},
	{0, "東京テック", "東京", "https://tokyo.example.com", "hi@tokyo.example.com", "", "", "Software", "11-50", "qualified", 55, "warm", nil},
	{1, "Initech", "Pune", "https://initech.example.com", "team@initech.example.com", "", "Bill Lumbergh", "Software", "51-200", "new", 70, "warm", nil},
	{1, "Hooli", "Palo Alto", "https://hooli.example.com", "press@hooli.example.com", "", "", "Software", "1000+", "new", 90, "hot", nil},
}

type readEnv struct {
	pool   *pgxpool.Pool
	d      Deps
	ws     [2]string
	ids    []int64 // real id of fixture id n is ids[n-1]
	server *Server
}

func newReadEnv(t *testing.T, maxConns int32) *readEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, maxConns)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	e := &readEnv{pool: pool, ws: [2]string{"t8-alpha-" + sfx, "t8-beta-" + sfx}}
	ctx := context.Background()
	for i, w := range e.ws {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar,$1::text,$1::text)`, w); err != nil {
			t.Fatal(i, err)
		}
	}
	t.Cleanup(func() {
		for _, w := range e.ws {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM leads`)
				return err
			})
			_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, w)
		}
	})
	for _, l := range seedLeads {
		w := e.ws[l.ws]
		var id int64
		err := tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, city, website, email, phone, contact_person, specialization,
				company_size, status, score, score_tier, decision_makers, source, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'mcp-seed','2026-08-29T10:00:00','2026-08-29T10:00:00') RETURNING id`,
				w, l.company, l.city, l.website, l.email, l.phone, l.person, l.spec, l.sz, l.status, l.score, l.tier, l.dm).Scan(&id)
		})
		if err != nil {
			t.Fatal(err)
		}
		e.ids = append(e.ids, id)
	}
	e.d = Deps{
		Cfg: Config{RequireAuth: true},
		InWorkspace: func(ctx context.Context, ws string, fn func(pgx.Tx) error) error {
			return tenancy.InWorkspace(ctx, pool, ws, fn)
		},
		Leads: &pgLeads{},
	}
	e.server = NewServer(e.d, NewRegistry(readTools(e.d), nil))
	return e
}

func (e *readEnv) ctxFor(alias string) Ctx {
	w := e.ws[0]
	if alias == "workspace_b" {
		w = e.ws[1]
	}
	return Ctx{WorkspaceID: w, Capabilities: map[string]bool{"leads:read": true}}
}

var idRe = regexp.MustCompile(`"id": (\d+)`)

// norm maps recorded seed ids and workspace names to this run's.
func (e *readEnv) norm(s string) string {
	s = idRe.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.Atoi(idRe.FindStringSubmatch(m)[1])
		if n >= 1 && n <= len(e.ids) {
			return fmt.Sprintf(`"id": %d`, e.ids[n-1])
		}
		return m
	})
	s = strings.ReplaceAll(s, `"workspace_id": "ws-alpha"`, `"workspace_id": "`+e.ws[0]+`"`)
	return strings.ReplaceAll(s, `"workspace_id": "ws-beta"`, `"workspace_id": "`+e.ws[1]+`"`)
}

func (e *readEnv) call(t *testing.T, c Ctx, name string, args map[string]any) string {
	t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	resp, ok := e.server.Handle(context.Background(), c, req)
	if !ok {
		t.Fatal("no response")
	}
	var r struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool `json:"isError"`
		}
	}
	if err := json.Unmarshal(resp, &r); err != nil || len(r.Result.Content) != 1 || r.Result.IsError {
		t.Fatalf("resp %s err %v", resp, err)
	}
	return r.Result.Content[0].Text
}

type toolFixture struct {
	Token   string `json:"token"`
	Request struct {
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	} `json:"request"`
	Response struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	} `json:"response"`
	Deviation string `json:"deviation"`
}

func (e *readEnv) replay(t *testing.T, pattern string) int {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(fixtureRoot, "tools", pattern))
	n := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx toolFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		if fx.Deviation != "" {
			continue
		}
		args := fx.Request.Params.Arguments
		if v, ok := args["lead_id"].(float64); ok && v >= 1 && int(v) <= len(e.ids) {
			args["lead_id"] = e.ids[int(v)-1]
		}
		got := e.call(t, e.ctxFor(fx.Token), fx.Request.Params.Name, args)
		want := e.norm(fx.Response.Result.Content[0].Text)
		if fx.Request.Params.Name == "get_pipeline_stats" {
			assertStatsEqual(t, filepath.Base(f), got, want)
		} else if got != want {
			t.Errorf("%s:\n got %s\nwant %s", filepath.Base(f), got, want)
		}
		n++
	}
	return n
}

func TestFindLeadsMatchesFixtures(t *testing.T) {
	e := newReadEnv(t, 4)
	if n := e.replay(t, "read__find_leads*.json"); n < 9 {
		t.Fatalf("replayed %d", n)
	}
}

func TestFindLeadsUnicodeCompanyFixture(t *testing.T) {
	e := newReadEnv(t, 4)
	if n := e.replay(t, "read__find_leads_unicode.json"); n != 1 {
		t.Fatalf("replayed %d", n)
	}
}

func TestGetLeadDetailMatchesFixture(t *testing.T) {
	e := newReadEnv(t, 4)
	if n := e.replay(t, "read__get_lead_detail*.json"); n < 6 {
		t.Fatalf("replayed %d", n)
	}
}

func TestGetPipelineStatsMatchesFixture(t *testing.T) {
	e := newReadEnv(t, 4)
	if n := e.replay(t, "read__get_pipeline_stats*.json"); n != 2 {
		t.Fatalf("replayed %d", n)
	}
}

func TestGetLeadDetailCrossTenantIsNotFound(t *testing.T) {
	e := newReadEnv(t, 4)
	beta := e.ids[6]
	if got := e.call(t, e.ctxFor("read"), "get_lead_detail", map[string]any{"lead_id": beta}); got != `{"error": "Lead not found"}` {
		t.Fatal(got)
	}
	if got := e.call(t, e.ctxFor("workspace_b"), "get_lead_detail", map[string]any{"lead_id": e.ids[0]}); got != `{"error": "Lead not found"}` {
		t.Fatal(got)
	}
}

func TestReadToolsRunScopedToTokenWorkspace(t *testing.T) {
	e := newReadEnv(t, 1) // one connection: a leaked GUC would show on the next call
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		a := e.call(t, e.ctxFor("read"), "find_leads", map[string]any{"query": "", "workspace_id": e.ws[1]})
		b := e.call(t, e.ctxFor("workspace_b"), "find_leads", map[string]any{"query": ""})
		if strings.Contains(a, "Initech") || strings.Contains(a, "Hooli") || !strings.Contains(a, "Acme Robotics") {
			t.Fatalf("alpha leaked: %s", a)
		}
		if strings.Contains(b, "Acme") || !strings.Contains(b, "Hooli") || !strings.Contains(b, `"count": 2`) {
			t.Fatalf("beta wrong: %s", b)
		}
		st := e.call(t, e.ctxFor("read"), "get_pipeline_stats", nil)
		if !strings.Contains(st, `"total": 6`) {
			t.Fatalf("stats: %s", st)
		}
	}
	var guc *string
	if err := e.pool.QueryRow(ctx, `SELECT NULLIF(current_setting('app.workspace_id', true), '')`).Scan(&guc); err != nil || guc != nil {
		t.Fatalf("GUC leaked on the pooled connection: %v %v", guc, err)
	}
	// without a transaction scope the leads table shows nothing
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM leads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unscoped read saw %d rows (%v)", n, err)
	}
}

// assertStatsEqual compares get_pipeline_stats payloads. Python builds the by_*
// dicts in the unspecified GROUP BY order of its database (a hash-aggregate
// artifact); Go uses plan 05's deterministic order (key, or count desc then
// city). So the group buckets compare as maps while everything else, the
// top-level and enrichment key order and every scalar's text, is exact.
func assertStatsEqual(t *testing.T, name, got, want string) {
	t.Helper()
	groups := []string{"by_status", "by_tier", "by_city", "by_source"}
	strip := func(s string) (map[string]any, string) {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("%s: %v in %s", name, err, s)
		}
		sub := map[string]any{}
		for _, g := range groups {
			sub[g] = m[g]
		}
		// blank each group's body so only the key order and scalars remain
		for _, g := range groups {
			re := regexp.MustCompile(`(?s)("` + g + `": )\{.*?\}`)
			s = re.ReplaceAllString(s, "${1}{}")
		}
		return sub, s
	}
	gm, gs := strip(got)
	wm, ws := strip(want)
	if gs != ws {
		t.Errorf("%s: shape differs:\n got %s\nwant %s", name, gs, ws)
	}
	if fmt.Sprint(gm) != fmt.Sprint(wm) { // fmt prints maps in sorted key order
		t.Errorf("%s: buckets differ:\n got %v\nwant %v", name, gm, wm)
	}
}
