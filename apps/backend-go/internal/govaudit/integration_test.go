//go:build integration

package govaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool, owner *pgxpool.Pool
	prefix      string
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{pool: testpg.NewAppPool(t, 8), prefix: fmt.Sprintf("t4_%d_", os.Getpid())}
	var err error
	e.owner, err = pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.owner.Close)
	e.clean()
	t.Cleanup(e.clean)
	return e
}

func (e *env) clean() {
	ctx := context.Background()
	_, _ = e.owner.Exec(ctx, `DELETE FROM governance_audit_events WHERE workspace_id LIKE $1`, e.prefix+"%")
	_, _ = e.owner.Exec(ctx, `DELETE FROM workspaces WHERE id LIKE $1`, e.prefix+"%")
}

func (e *env) workspace(t *testing.T, name string) string {
	t.Helper()
	id := e.prefix + name
	if _, err := e.owner.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ($1::text,$1::text,$1::text, extract(epoch from clock_timestamp()), extract(epoch from now()))`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) event(t *testing.T, ws, id, method, outcome string, actor *int32, at time.Time, meta string) {
	t.Helper()
	if _, err := e.owner.Exec(context.Background(),
		`INSERT INTO governance_audit_events (id, workspace_id, actor_user_id, actor_role, method, route, resource_path, response_status, outcome, request_id, metadata_json, created_at)
		 VALUES ($1,$2,$3,'admin',$4,'/r','/p',200,$5,'rid',$6::json,$7)`, id, ws, actor, method, outcome, meta, at); err != nil {
		t.Fatal(err)
	}
}

func list(t *testing.T, e *env, ws string, f Filter, cursor string, limit int) Page {
	t.Helper()
	var p Page
	var cerr error
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		p, cerr = List(context.Background(), tx, ws, f, cursor, limit)
		return nil
	})
	if err != nil || cerr != nil {
		t.Fatalf("list: %v %v", err, cerr)
	}
	return p
}

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func ids(p Page) string {
	var s []string
	for _, e := range p.Events {
		s = append(s, e.ID)
	}
	return strings.Join(s, ",")
}

func TestListOrderTiesByIDDesc(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	e.event(t, ws, "a", "GET", "success", nil, base, "{}")
	e.event(t, ws, "c", "GET", "success", nil, base, "{}")
	e.event(t, ws, "b", "GET", "success", nil, base, "{}")
	e.event(t, ws, "z", "GET", "success", nil, base.Add(-time.Hour), "{}")
	if got := ids(list(t, e, ws, Filter{}, "", 100)); got != "c,b,a,z" {
		t.Fatalf("got %s", got)
	}
}

func TestListHasMoreAndNextCursor(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	for i := 0; i < 5; i++ {
		e.event(t, ws, fmt.Sprintf("e%d", i), "GET", "success", nil, base.Add(time.Duration(i)*time.Second), "{}")
	}
	p := list(t, e, ws, Filter{}, "", 2)
	if ids(p) != "e4,e3" || !p.HasMore || p.NextCursor == nil || p.NextBefore == nil || p.Limit != 2 {
		t.Fatalf("%+v", p)
	}
	if *p.NextCursor != EncodeCursor(base.Add(3*time.Second), "e3") || !p.NextBefore.Equal(base.Add(3*time.Second)) {
		t.Fatalf("cursor %s", *p.NextCursor)
	}
	p2 := list(t, e, ws, Filter{}, *p.NextCursor, 2)
	if ids(p2) != "e2,e1" || !p2.HasMore {
		t.Fatalf("%+v", p2)
	}
	p3 := list(t, e, ws, Filter{}, *p2.NextCursor, 2)
	if ids(p3) != "e0" || p3.HasMore || p3.NextCursor != nil || p3.NextBefore != nil {
		t.Fatalf("%+v", p3)
	}
	// limit is capped
	if p := list(t, e, ws, Filter{}, "", 100000); p.Limit != 500 {
		t.Fatalf("limit %d", p.Limit)
	}
}

func TestListCursorIgnoresBefore(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	for i := 0; i < 3; i++ {
		e.event(t, ws, fmt.Sprintf("e%d", i), "GET", "success", nil, base.Add(time.Duration(i)*time.Second), "{}")
	}
	early := base.Add(-time.Hour)
	cur := EncodeCursor(base.Add(2*time.Second), "e2")
	if got := ids(list(t, e, ws, Filter{Before: &early}, cur, 10)); got != "e1,e0" {
		t.Fatalf("got %s", got)
	}
	if got := ids(list(t, e, ws, Filter{Before: &early}, "", 10)); got != "" {
		t.Fatalf("got %s", got)
	}
}

func TestListFiltersMethodUppercase(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	u := int32(7)
	e.event(t, ws, "a", "POST", "success", &u, base, "{}")
	e.event(t, ws, "b", "GET", "denied", nil, base.Add(time.Second), "{}")
	if got := ids(list(t, e, ws, Filter{Method: "post"}, "", 10)); got != "a" {
		t.Fatalf("method %s", got)
	}
	if got := ids(list(t, e, ws, Filter{Outcome: "denied"}, "", 10)); got != "b" {
		t.Fatalf("outcome %s", got)
	}
	if got := ids(list(t, e, ws, Filter{ActorUserID: &u}, "", 10)); got != "a" {
		t.Fatalf("actor %s", got)
	}
	since := base.Add(time.Second)
	if got := ids(list(t, e, ws, Filter{Since: &since}, "", 10)); got != "b" {
		t.Fatalf("since %s", got)
	}
	if got := ids(list(t, e, ws, Filter{Before: &since}, "", 10)); got != "a" {
		t.Fatalf("before %s", got)
	}
}

func TestListTenantIsolation(t *testing.T) {
	e := setup(t)
	a, b := e.workspace(t, "a"), e.workspace(t, "b")
	e.event(t, a, "ea", "GET", "success", nil, base, "{}")
	e.event(t, b, "eb", "GET", "success", nil, base, "{}")
	if got := ids(list(t, e, a, Filter{}, "", 10)); got != "ea" {
		t.Fatalf("got %s", got)
	}
	// ws argument is a filter too: asking for b inside a's transaction finds nothing.
	err := tenancy.InWorkspace(context.Background(), e.pool, a, func(tx pgx.Tx) error {
		p, _ := List(context.Background(), tx, b, Filter{}, "", 10)
		if len(p.Events) != 0 {
			t.Fatalf("leak: %+v", p.Events)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ExportCSV(context.Background(), e.pool, b, Filter{}, &out); err != nil || strings.Contains(out.String(), "\r\nea,") {
		t.Fatalf("export leak %v %q", err, out.String())
	}
}

func TestListMetadataNormalized(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	e.event(t, ws, "a", "GET", "success", nil, base, `{"z": "caf\u00e9",  "a": 1.50}`)
	p := list(t, e, ws, Filter{}, "", 10)
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"metadata":{"z":"café","a":1.5}`) || !strings.Contains(string(b), `"next_cursor":null,"next_before":null,"has_more":false,"limit":10`) {
		t.Fatalf("%s", b)
	}
}

func TestExportCSVBytesMatchFixture(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	raw, _ := os.ReadFile("testdata/export_fixture.json")
	var rows []struct {
		ID             string `json:"id"`
		CreatedAt      string `json:"created_at"`
		ActorRole      string `json:"actor_role"`
		Method         string `json:"method"`
		Route          string `json:"route"`
		ResourcePath   string `json:"resource_path"`
		Outcome        string `json:"outcome"`
		RequestID      string `json:"request_id"`
		ActorUserID    *int32 `json:"actor_user_id"`
		ResponseStatus int    `json:"response_status"`
		MetadataText   string `json:"metadata_json_text"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		at, _ := parseISO(r.CreatedAt)
		if _, err := e.owner.Exec(context.Background(),
			`INSERT INTO governance_audit_events (id, workspace_id, actor_user_id, actor_role, method, route, resource_path, response_status, outcome, request_id, metadata_json, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::json,$12)`,
			r.ID, ws, r.ActorUserID, r.ActorRole, r.Method, r.Route, r.ResourcePath, r.ResponseStatus, r.Outcome, r.RequestID, r.MetadataText, at.Format("2006-01-02 15:04:05.999999")); err != nil {
			t.Fatal(err)
		}
	}
	want, _ := os.ReadFile("testdata/export_fixture.csv")
	var out bytes.Buffer
	if err := ExportCSV(context.Background(), e.pool, ws, Filter{}, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("csv mismatch\n got %q\nwant %q", out.Bytes(), want)
	}
}

type countWriter struct {
	maxWrite, n int
	failAfter   int
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.n++
	if len(p) > c.maxWrite {
		c.maxWrite = len(p)
	}
	if c.failAfter > 0 && c.n >= c.failAfter {
		return 0, fmt.Errorf("client gone")
	}
	return len(p), nil
}

func TestExportCSVStreamsInBatches(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "a")
	if _, err := e.owner.Exec(context.Background(),
		`INSERT INTO governance_audit_events (id, workspace_id, actor_role, method, route, resource_path, response_status, outcome, request_id, metadata_json, created_at)
		 SELECT 'e' || lpad(g::text,5,'0'), $1, 'admin','GET','/r','/p',200,'success','rid','{}'::json, timestamp '2026-03-01 00:00:00' + g * interval '1 second' FROM generate_series(1,3000) g`, ws); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	batchHook = func(n int) { sizes = append(sizes, n) }
	defer func() { batchHook = nil }()
	var out bytes.Buffer
	if err := ExportCSV(context.Background(), e.pool, ws, Filter{}, &out); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sizes) != "[500 500 500 500 500 500]" {
		t.Fatalf("batches %v", sizes)
	}
	if n := strings.Count(out.String(), "\r\n"); n != 3001 {
		t.Fatalf("lines %d", n)
	}
	// Client disconnect stops the export without reading further batches.
	sizes = nil
	cw := &countWriter{failAfter: 3}
	if err := ExportCSV(context.Background(), e.pool, ws, Filter{}, cw); err == nil {
		t.Fatal("want write error")
	}
	if len(sizes) > 1 {
		t.Fatalf("kept reading after write failure: %v", sizes)
	}
}

// Python sends aware since/before and every decoded cursor as timestamptz, so
// Postgres reads the naive column in the session TimeZone; Go must do the same.
func TestListSessionTimeZoneMatchesPython(t *testing.T) {
	e := setup(t)
	ws := e.workspace(t, "tz")
	for i, h := range []int{10, 12, 14} {
		e.event(t, ws, fmt.Sprintf("e%d", i), "GET", "success", nil, time.Date(2026, 3, 1, h, 0, 0, 0, time.UTC), "{}")
	}
	run := func(f Filter, cursor string) string {
		var p Page
		err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `SET LOCAL TIME ZONE 'Asia/Kolkata'`); err != nil {
				return err
			}
			var lerr error
			p, lerr = List(context.Background(), tx, ws, f, cursor, 10)
			return lerr
		})
		if err != nil {
			t.Fatal(err)
		}
		return ids(p)
	}
	kolkata := time.FixedZone("", 19800)
	// aware 11:30+05:30 == 11:30 IST wall clock == naive 11:30 -> rows at 12:00 and 14:00
	s := time.Date(2026, 3, 1, 11, 30, 0, 0, kolkata)
	if got := run(Filter{Since: &s}, ""); got != "e2,e1" {
		t.Errorf("aware since (offset equal to session tz): %s", got)
	}
	// aware 06:00Z == 11:30 IST wall clock: same rows (UTC conversion would return all three)
	z := time.Date(2026, 3, 1, 6, 0, 0, 0, time.FixedZone("", 0))
	if got := run(Filter{Since: &z}, ""); got != "e2,e1" {
		t.Errorf("aware since Z: %s", got)
	}
	if got := run(Filter{Before: &z}, ""); got != "e0" {
		t.Errorf("aware before Z: %s", got)
	}
	// naive stays a plain wall-clock compare
	n := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	if got := run(Filter{Since: &n}, ""); got != "e2,e1" {
		t.Errorf("naive since: %s", got)
	}
	// Python decodes a naive cursor as UTC-aware: 12:00Z == 17:30 IST, so all rows sort before it
	cur := EncodeCursor(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), "zzz")
	if got := run(Filter{}, cur); got != "e2,e1,e0" {
		t.Errorf("naive cursor: %s", got)
	}
	// an explicit-offset cursor 12:00+05:30 == 12:00 IST wall clock: ties on e1 resolved by id
	cur = EncodeCursor(time.Date(2026, 3, 1, 12, 0, 0, 0, kolkata), "e1")
	if got := run(Filter{}, cur); got != "e0" {
		t.Errorf("aware cursor: %s", got)
	}
}
