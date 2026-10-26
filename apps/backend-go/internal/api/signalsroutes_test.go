//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type sigEnv struct{ mcpEnv }

const sigFixedTS = 1788000000.0

var sigOffsets = []float64{
	0.0, 0.0, 0.0, -3600.0, -3600.0, 10800.0, 10800.0, 18000.5, -7200.0, -7200.0, -7200.0, -86400.0, -86400.0,
	-90000.123456, -172800.0, -172800.0, -172800.0, -172800.0, -259200.0, 43200.0, 43200.0, -345600.0, -432000.0,
	-432000.0, -518400.0, 3.5, 7.25, -604800.0, -604800.0, -1209600.0,
}

func newSigEnv(t *testing.T) sigEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}}
	RegisterSignals(r, d)
	e := sigEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge()
	t.Cleanup(e.purge)
	e.seedSignals(t)
	return e
}

func (e sigEnv) purge() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "DELETE FROM signals")
			return err
		})
	}
	_, _ = e.pool.Exec(ctx, `DELETE FROM jobs WHERE type='signal_scan'`)
}

func (e sigEnv) insertSignal(t *testing.T, ws, id string, lead int, company, typ, title, desc, src, url string, weight int, at float64, read bool) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO signals (id, workspace_id, lead_id, company, signal_type, title, description, source, source_url, weight, created_at, read)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, ws, lead, company, typ, title, desc, src, url, weight, at, read)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// seedSignals mirrors contract_seed.py: 30 alpha signals (ties, a NULL-free set) and 6 beta signals.
func (e sigEnv) seedSignals(t *testing.T) {
	t.Helper()
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	types := []string{"hiring", "funding", "tech_change", "news", "growth"}
	tw := map[string]int{"hiring": 8, "funding": 10, "tech_change": 6, "news": 5, "growth": 7}
	var leads []int
	for _, g := range [][2]int{{14, 10}, {6, 7}, {10, 5}, {4, 4}, {0, 4}} {
		for i := 0; i < g[1]; i++ {
			leads = append(leads, g[0])
		}
	}
	companies := map[int]string{14: "Hot Pursuit Labs", 6: "Zenith Robotics Pvt Ltd", 10: "Quantum Foods", 4: "Éco Solutions Pvt Ltd", 0: ""}
	for k := 1; k <= 30; k++ {
		ty := types[k%5]
		title := fmt.Sprintf("%s signal %d", ty, k)
		if k%7 == 0 {
			title = ""
		}
		e.insertSignal(t, "ws-alpha", fmt.Sprintf("sig-a-%02d", k), leads[k-1], companies[leads[k-1]], ty, title,
			fmt.Sprintf("Seeded %s event #%d for contract fixtures", ty, k), []string{"jobspy", "newsapi", "crunchbase"}[k%3],
			fmt.Sprintf("https://signals.example.com/a/%d", k), tw[ty]+k%3, sigFixedTS+sigOffsets[k-1], k%3 == 0)
	}
	for k := 1; k <= 6; k++ {
		ty := types[k%5]
		lead := 18
		if k%2 == 1 {
			lead = 17
		}
		e.insertSignal(t, "ws-beta", fmt.Sprintf("sig-b-%02d", k), lead, map[bool]string{true: "Zenith Robotics Pvt Ltd", false: "Initech Two"}[k%2 == 1], ty,
			fmt.Sprintf("beta %s %d", ty, k), "beta tenant signal", "jobspy", fmt.Sprintf("https://signals.example.com/b/%d", k), tw[ty], sigFixedTS-float64(k)*1800, false)
	}
}

func (e sigEnv) req(method, path, who, ws, body string) (*httptest.ResponseRecorder, contract.Resp) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	return e.do(method, path, ct, body, hdr)
}

func (e sigEnv) get(who, ws, path string) *httptest.ResponseRecorder {
	rec, _ := e.req("GET", path, who, ws, "")
	return rec
}

func (e sigEnv) readCount(t *testing.T, ws string) (read int) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM signals WHERE read`).Scan(&read)
	})
	if err != nil {
		t.Fatal(err)
	}
	return read
}

// keyOrder returns the top-level keys of a JSON object in document order.
func keyOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestListSignalsOffsetShape(t *testing.T) {
	e := newSigEnv(t)
	rec := e.get("alice", "", "/api/signals?limit=5&offset=10")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	want := []string{"signals", "has_more", "next_cursor", "limit", "offset", "counts", "signal_types"}
	if got := keyOrder(t, rec.Body.Bytes()); !eq(got, want) {
		t.Errorf("keys %v", got)
	}
	var b struct {
		Signals []json.RawMessage `json:"signals"`
		HasMore bool              `json:"has_more"`
		Next    *string           `json:"next_cursor"`
		Offset  *int              `json:"offset"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if len(b.Signals) != 5 || !b.HasMore || b.Next == nil || b.Offset == nil || *b.Offset != 10 {
		t.Errorf("page %+v", b)
	}
}

func TestListSignalsCursorShape(t *testing.T) {
	e := newSigEnv(t)
	var p1 struct {
		Next string `json:"next_cursor"`
	}
	_ = json.Unmarshal(e.get("alice", "", "/api/signals?limit=4").Body.Bytes(), &p1)
	rec := e.get("alice", "", "/api/signals?limit=4&offset=7&cursor="+p1.Next)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	want := []string{"signals", "next_cursor", "has_more", "limit", "offset", "counts", "signal_types"}
	if got := keyOrder(t, rec.Body.Bytes()); !eq(got, want) {
		t.Errorf("keys %v", got)
	}
	if !regexp.MustCompile(`"offset":null`).Match(rec.Body.Bytes()) {
		t.Errorf("offset not null: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"signal_types":{"hiring":"Hiring","funding":"Funding","tech_change":"Tech Change","website_change":"Website Update","news":"News Mention","growth":"Growth Signal","job_change":"Job Change","social_activity":"Social Activity"}`) {
		t.Errorf("signal_types order: %s", rec.Body.String())
	}
}

func TestListSignalsBadCursor422(t *testing.T) {
	e := newSigEnv(t)
	for _, c := range []string{"!!!", "aGVsbG8", "WzEsMl0", "WzE3ODgwMDAwMDAuMCw1XQ", "eyJhIjoxfQ"} {
		rec := e.get("alice", "", "/api/signals?cursor="+c)
		if rec.Code != 422 || rec.Body.String() != `{"detail":"invalid signal cursor"}` {
			t.Errorf("%s: %d %s", c, rec.Code, rec.Body.String())
		}
	}
	for _, c := range []string{"W05hTiwiYSJd", "W0luZmluaXR5LCJhIl0"} { // NaN / Infinity: never 500
		if rec := e.get("alice", "", "/api/signals?cursor="+c); rec.Code >= 500 {
			t.Errorf("%s: %d", c, rec.Code)
		}
	}
}

func TestListSignalsLimitBounds422(t *testing.T) {
	e := newSigEnv(t)
	for _, p := range []string{"limit=0", "limit=201", "limit=x", "offset=-1", "lead_id=abc"} {
		if rec := e.get("alice", "", "/api/signals?"+p); rec.Code != 422 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := e.get("alice", "", "/api/signals?limit=200"); rec.Code != 200 {
		t.Errorf("limit=200: %d", rec.Code)
	}
}

func TestSignalsCountsContract(t *testing.T) {
	e := newSigEnv(t)
	rec := e.get("alice", "", "/api/signals/counts")
	want := `{"funding":6,"growth":6,"hiring":6,"news":6,"tech_change":6,"total":30,"unread":20}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestMarkReadCountEchoesRequest(t *testing.T) {
	e := newSigEnv(t)
	rec, _ := e.req("POST", "/api/signals/mark-read", "erin", "ws-alpha", `{"signal_ids":["sig-a-01","sig-a-01","missing"]}`)
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok","count":3}` {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	rec, _ = e.req("POST", "/api/signals/mark-read", "erin", "ws-alpha", `{"signal_ids":[]}`)
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok","count":0}` {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{}`, `{"signal_ids":[1,2]}`, `{"signal_ids":null}`, `[]`, ``} {
		if rec, _ := e.req("POST", "/api/signals/mark-read", "erin", "ws-alpha", body); rec.Code != 422 {
			t.Errorf("%q: %d", body, rec.Code)
		}
	}
}

func TestMarkReadViewerForbidden(t *testing.T) {
	e := newSigEnv(t)
	before := e.readCount(t, "ws-alpha")
	rec, _ := e.req("POST", "/api/signals/mark-read", "bob", "ws-alpha", `{"signal_ids":["sig-a-01"]}`)
	if rec.Code != 403 || rec.Body.String() != `{"detail":"Workspace permission denied: signals.write"}` {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	if e.readCount(t, "ws-alpha") != before {
		t.Error("viewer changed data")
	}
}

func TestMarkReadForeignIdsUntouched(t *testing.T) {
	e := newSigEnv(t)
	before := e.readCount(t, "ws-beta")
	rec, _ := e.req("POST", "/api/signals/mark-read", "erin", "ws-alpha", `{"signal_ids":["sig-b-01","sig-b-02","sig-a-01"]}`)
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok","count":3}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if after := e.readCount(t, "ws-beta"); after != before {
		t.Errorf("beta read %d -> %d", before, after)
	}
	if e.readCount(t, "ws-alpha") != 11 { // 10 seeded read + sig-a-01
		t.Errorf("alpha read %d", e.readCount(t, "ws-alpha"))
	}
}

func TestAnalyticsContract(t *testing.T) {
	e := newSigEnv(t)
	e.purge()
	now := float64(time.Now().Unix())
	e.insertSignal(t, "ws-alpha", "an-1", 14, "Acme", "hiring", "t", "d", "s", "u", 8, now-3600, false)
	e.insertSignal(t, "ws-alpha", "an-2", 14, "Acme", "funding", "t", "d", "s", "u", 10, now-7200, false)
	e.insertSignal(t, "ws-alpha", "an-3", 6, "", "hiring", "t", "d", "s", "u", 8, now-3*86400, false)
	e.insertSignal(t, "ws-alpha", "an-old", 6, "", "hiring", "t", "d", "s", "u", 8, now-8*86400-5, false) // previous window of days=7
	rec := e.get("alice", "", "/api/signals/analytics?days=7")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	want := []string{"period_days", "summary", "trend", "by_type", "by_source", "top_accounts"}
	if got := keyOrder(t, rec.Body.Bytes()); !eq(got, want) {
		t.Errorf("keys %v", got)
	}
	var b struct {
		Summary map[string]any   `json:"summary"`
		Trend   []map[string]any `json:"trend"`
		Top     []map[string]any `json:"top_accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Summary["total"] != float64(3) || b.Summary["weighted_score"] != float64(26) || b.Summary["active_accounts"] != float64(2) {
		t.Errorf("summary %v", b.Summary)
	}
	if b.Summary["momentum_pct"] != float64(200) { // (3-1)/1*100
		t.Errorf("momentum %v", b.Summary["momentum_pct"])
	}
	if len(b.Trend) != 7 || len(b.Top) != 2 || b.Top[0]["company"] != "Acme" || b.Top[1]["company"] != "Lead 6" {
		t.Errorf("trend %d top %v", len(b.Trend), b.Top)
	}
	if !strings.Contains(rec.Body.String(), `"momentum_pct":200.0`) {
		t.Errorf("momentum repr: %s", rec.Body.String())
	}
}

func TestAnalyticsDaysBounds422(t *testing.T) {
	e := newSigEnv(t)
	for _, p := range []string{"days=6", "days=91", "days=x"} {
		if rec := e.get("alice", "", "/api/signals/analytics?"+p); rec.Code != 422 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"days=7", "days=90", ""} {
		if rec := e.get("alice", "", "/api/signals/analytics?"+p); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestScanEnqueuesSignalScan(t *testing.T) {
	e := newSigEnv(t)
	rec, _ := e.req("POST", "/api/signals/scan", "alice", "", "")
	if rec.Code != 200 || rec.Body.String() != `{"status":"started"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var n int
	var payload string
	var ws, fire *string
	var exec string
	err := e.pool.QueryRow(context.Background(), `SELECT count(*), max(payload::text), max(executor) FROM jobs WHERE type='signal_scan'`).Scan(&n, &payload, &exec)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(context.Background(), `SELECT workspace_id, fire_key FROM jobs WHERE type='signal_scan'`).Scan(&ws, &fire)
	if n != 1 || payload != "{}" || exec != "python" || ws != nil || fire != nil {
		t.Errorf("job n=%d payload=%s exec=%s ws=%v fire=%v", n, payload, exec, ws, fire)
	}
}

func TestScanRequiresAdmin(t *testing.T) {
	e := newSigEnv(t)
	for _, who := range []string{"bob", "erin"} {
		rec, _ := e.req("POST", "/api/signals/scan", who, "", "")
		if rec.Code != 403 || rec.Body.String() != `{"detail":"Not authorized"}` {
			t.Errorf("%s: %d %s", who, rec.Code, rec.Body.String())
		}
	}
	if rec, _ := e.req("POST", "/api/signals/scan", "", "", ""); rec.Code != 401 {
		t.Errorf("no auth: %d", rec.Code)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE type='signal_scan'`).Scan(&n)
	if n != 0 {
		t.Errorf("%d jobs enqueued by non-admins", n)
	}
}

func TestSignalsTenantIsolation(t *testing.T) {
	e := newSigEnv(t)
	rec := e.get("bob", "ws-beta", "/api/signals?limit=200")
	var b struct {
		Signals []struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"signals"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if len(b.Signals) != 6 {
		t.Fatalf("%d signals", len(b.Signals))
	}
	for _, s := range b.Signals {
		if s.WorkspaceID != "ws-beta" {
			t.Fatalf("leak %s", s.WorkspaceID)
		}
	}
	if rec := e.get("alice", "ws-beta", "/api/signals"); rec.Code != 403 {
		t.Errorf("non-member: %d", rec.Code)
	}
}

func sigActor(name string) (who, ws string) {
	switch name {
	case "list_beta_bob", "counts_beta", "analytics_beta", "counts_beta_after_mark":
		return "bob", "ws-beta"
	case "list_viewer_bob_alpha", "mark_read_viewer":
		return "bob", "ws-alpha"
	case "list_nonmember", "mark_read_nonmember":
		return "alice", "ws-beta"
	case "scan_non_admin":
		return "bob", ""
	case "scan_editor_non_admin", "mark_read_ok", "mark_read_empty", "mark_read_duplicates", "mark_read_missing_field", "mark_read_bad_type":
		return "erin", ""
	}
	if strings.HasSuffix(name, "_no_auth") {
		return "", ""
	}
	return "alice", ""
}

func TestSignalFixturesMatchPython(t *testing.T) {
	e := newSigEnv(t)
	// Reads run on the seed; the mutating cases follow in recorded order.
	tail := []string{"scan_admin", "scan_non_admin", "scan_editor_non_admin", "scan_no_auth", "mark_read_ok", "counts_after_mark",
		"counts_beta_after_mark", "mark_read_empty", "mark_read_duplicates", "mark_read_missing_field", "mark_read_bad_type",
		"mark_read_admin_owner", "mark_read_viewer", "mark_read_nonmember", "mark_read_no_auth", "list_after_mark"}
	isTail := map[string]bool{}
	for _, n := range tail {
		isTail[n] = true
	}
	var order []string
	for _, full := range contractFixtureNames(t, "signals") {
		if n := strings.TrimPrefix(full, "signals/"); !isTail[n] {
			order = append(order, n)
		}
	}
	order = append(order, tail...)
	for _, name := range order {
		fx := contract.Load(t, "signals/"+name)
		who, ws := sigActor(name)
		body := ""
		if fx.Request.Body != nil && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		rec, got := e.req(fx.Request.Method, fx.Request.Path, who, ws, body)
		_ = rec
		var opts []contract.Option
		if strings.HasPrefix(name, "analytics_") && fx.Response.Status == 200 {
			// time dependent: trend dates, windows and counts move with now
			opts = append(opts, contract.IgnoreShapeOf("$.summary", "$.trend", "$.by_type", "$.by_source", "$.top_accounts"))
		}
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	}
}
