//go:build integration

package destinbound_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsA    = "p10t10-ws-a"
	wsB    = "p10t10-ws-b"
	audA   = "p10t10-aud-a"
	audB   = "p10t10-aud-b"
	destA  = "p10t10-dest-a"  // dest-1 in the reconcile fixtures
	destA2 = "p10t10-dest-a2" // dest-2
	destB  = "p10t10-dest-b"
	runA   = "p10t10-run-a"
	runB   = "p10t10-run-b"
)

var allCols = []string{"company", "website", "email", "phone", "contact_person", "contact_title", "city", "state",
	"address", "specialization", "company_size", "employee_count_exact", "description", "revenue_range", "founded_year",
	"industry_tags", "technologies", "funding_stage", "linkedin_url", "twitter_url", "facebook_url", "secondary_emails",
	"secondary_phones", "decision_makers", "status", "notes"}

type env struct {
	app   *pgxpool.Pool
	owner *pgxpool.Pool
}

func (e *env) clean() {
	ctx := context.Background()
	for _, s := range []string{
		`DELETE FROM destination_inbound_receipts WHERE workspace_id LIKE 'p10t10-%'`,
		`DELETE FROM destination_inbound_tokens WHERE workspace_id LIKE 'p10t10-%'`,
		`DELETE FROM destination_deliveries WHERE workspace_id LIKE 'p10t10-%'`,
		`DELETE FROM destination_runs WHERE workspace_id LIKE 'p10t10-%'`,
		`DELETE FROM audience_destinations WHERE workspace_id LIKE 'p10t10-%'`,
		`DELETE FROM audiences WHERE workspace_id LIKE 'p10t10-%'`,
		`DELETE FROM leads WHERE workspace_id LIKE 'p10t10-%'`,
		`DELETE FROM workspaces WHERE id LIKE 'p10t10-%'`,
	} {
		if _, err := e.owner.Exec(ctx, s); err != nil {
			panic(err)
		}
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	app := testpg.NewAppPool(t, 24)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	e := &env{app: app, owner: owner}
	e.clean()
	t.Cleanup(e.clean)
	for _, w := range []string{wsA, wsB} {
		mustExec(t, owner, `INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ($1::text,$1::text,$1::text, extract(epoch from clock_timestamp()), extract(epoch from now()))`, w)
	}
	for _, a := range [][2]string{{audA, wsA}, {audB, wsB}} {
		mustExec(t, owner, `INSERT INTO audiences (id, workspace_id, name, filters) VALUES ($1,$2,$1,'{}'::json)`, a[0], a[1])
	}
	for _, d := range [][3]string{{destA, audA, wsA}, {destA2, audA, wsA}, {destB, audB, wsB}} {
		mustExec(t, owner, `INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, config, field_map)
			VALUES ($1,$3,$2,$1,'hubspot','{}'::json,'{}'::json)`, d[0], d[1], d[2])
	}
	mustExec(t, owner, `INSERT INTO destination_runs (id, workspace_id, destination_id) VALUES ($1,$2,$3),($4,$5,$6)`, runA, wsA, destA, runB, wsB, destB)
	return e
}

func mustExec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) in(t *testing.T, ws string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), e.app, ws, fn); err != nil {
		t.Fatal(err)
	}
}

var frozen = time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

// ---- tokens -------------------------------------------------------------

func (e *env) seedToken(t *testing.T, id, ws, dest, raw string, expires *time.Time, revoked *time.Time) {
	t.Helper()
	mustExec(t, e.owner, `INSERT INTO destination_inbound_tokens (id, workspace_id, destination_id, token_hash, prefix, expires_at, revoked_at)
		VALUES ($1,$2,$3,$4,'dsi_seed',$5,$6)`, id, ws, dest, destinbound.HashToken(raw), expires, revoked)
}

func TestRotateRevokesPrevious(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var first, second destinbound.RotateResult
	e.in(t, wsA, func(tx pgx.Tx) (err error) {
		first, err = destinbound.Rotate(ctx, tx, wsA, destA, 7, frozen)
		return
	})
	e.in(t, wsA, func(tx pgx.Tx) (err error) {
		second, err = destinbound.Rotate(ctx, tx, wsA, destA, 7, frozen.Add(time.Hour))
		return
	})
	if first.Token == second.Token || !strings.HasPrefix(second.Token, "dsi_") || second.Prefix != second.Token[:12] {
		t.Fatalf("bad rotate results")
	}
	if !second.ExpiresAt.Equal(frozen.Add(time.Hour).Add(90 * 24 * time.Hour)) {
		t.Fatalf("expires_at %v", second.ExpiresAt)
	}
	// Only the hash is stored; the raw token appears nowhere in the table.
	var n int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM destination_inbound_tokens WHERE token_hash=$1 OR prefix=$2`,
		destinbound.HashToken(second.Token), second.Prefix).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hash/prefix rows = %d err %v", n, err)
	}
	var leaked int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM destination_inbound_tokens t WHERE t::text LIKE '%'||$1||'%' OR t::text LIKE '%'||$2||'%'`, first.Token, second.Token).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("raw token stored: %d %v", leaked, err)
	}
	// First is revoked, second active; first no longer resolves, second does.
	if _, err := destinbound.Resolve(ctx, e.app, first.Token, destA, frozen.Add(2*time.Hour)); !errors.Is(err, destinbound.ErrInvalidToken) {
		t.Fatalf("revoked token resolved: %v", err)
	}
	ws, err := destinbound.Resolve(ctx, e.app, second.Token, destA, frozen.Add(2*time.Hour))
	if err != nil || ws != wsA {
		t.Fatalf("active token: %q %v", ws, err)
	}
	var active, revoked int
	_ = e.owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL), count(*) FILTER (WHERE revoked_at IS NOT NULL) FROM destination_inbound_tokens WHERE destination_id=$1`, destA).Scan(&active, &revoked)
	if active != 1 || revoked != 1 {
		t.Fatalf("active=%d revoked=%d", active, revoked)
	}
	// Rotating on destination B does not touch destination A.
	e.in(t, wsB, func(tx pgx.Tx) error { _, err := destinbound.Rotate(ctx, tx, wsB, destB, 1, frozen); return err })
	_ = e.owner.QueryRow(ctx, `SELECT count(*) FROM destination_inbound_tokens WHERE destination_id=$1 AND revoked_at IS NULL`, destA).Scan(&active)
	if active != 1 {
		t.Fatalf("destination A active tokens = %d", active)
	}
	// Revoke.
	var n64 int64
	e.in(t, wsA, func(tx pgx.Tx) (err error) { n64, err = destinbound.Revoke(ctx, tx, wsA, destA, frozen); return })
	if n64 != 1 {
		t.Fatalf("revoked %d", n64)
	}
	if _, err := destinbound.Resolve(ctx, e.app, second.Token, destA, frozen); !errors.Is(err, destinbound.ErrInvalidToken) {
		t.Fatalf("token still valid after revoke: %v", err)
	}
}

func TestStatusShapes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	status := func(now time.Time) destinbound.TokenStatus {
		var st destinbound.TokenStatus
		e.in(t, wsA, func(tx pgx.Tx) (err error) { st, err = destinbound.Status(ctx, tx, wsA, destA, now); return })
		return st
	}
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if got := js(status(frozen)); got != `{"active":false}` {
		t.Fatalf("none: %s", got)
	}
	var r destinbound.RotateResult
	e.in(t, wsA, func(tx pgx.Tx) (err error) { r, err = destinbound.Rotate(ctx, tx, wsA, destA, 42, frozen); return })
	st := status(frozen.Add(time.Hour))
	if !st.Has || !st.Active || st.Expired || st.Prefix != r.Prefix || st.CreatedBy == nil || *st.CreatedBy != 42 || st.LastUsedAt != nil {
		t.Fatalf("active: %+v", st)
	}
	var shape map[string]any
	if err := json.Unmarshal([]byte(js(st)), &shape); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"active", "expired", "prefix", "created_at", "expires_at", "last_used_at", "created_by"} {
		if _, ok := shape[k]; !ok {
			t.Errorf("status JSON lacks %q: %v", k, shape)
		}
	}
	if !strings.HasSuffix(shape["expires_at"].(string), "+00:00") || strings.HasSuffix(shape["created_at"].(string), "+00:00") {
		t.Errorf("python isoformat: %v %v", shape["created_at"], shape["expires_at"])
	}
	// The raw token never appears in the status.
	if strings.Contains(js(st), r.Token) {
		t.Fatal("status leaks the raw token")
	}
	exp := status(frozen.Add(91 * 24 * time.Hour))
	if !exp.Has || exp.Active || !exp.Expired {
		t.Fatalf("expired: %+v", exp)
	}
	// Another workspace sees nothing (and cannot be asked about foreign tenants).
	e.in(t, wsB, func(tx pgx.Tx) error {
		st, err := destinbound.Status(ctx, tx, wsB, destA, frozen)
		if err != nil || st.Has {
			t.Fatalf("foreign status: %+v %v", st, err)
		}
		return nil
	})
	mustExec(t, e.owner, `UPDATE destination_inbound_tokens SET expires_at = NULL WHERE destination_id=$1`, destA)
	if st := status(frozen); !st.Has || st.Active || !st.Expired || st.ExpiresAt != nil {
		t.Fatalf("no expiry: %+v", st)
	}
	e.in(t, wsA, func(tx pgx.Tx) error { _, err := destinbound.Revoke(ctx, tx, wsA, destA, frozen); return err })
	if st := status(frozen); st.Has {
		t.Fatalf("revoked still shown: %+v", st)
	}
}

func TestResolveRejectsUnknownExpiredRevokedForeignDestination(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	future, past, revAt := frozen.Add(24*time.Hour), frozen.Add(-24*time.Hour), frozen.Add(-time.Hour)
	e.seedToken(t, "t-active", wsA, destA, "raw-active", &future, nil)
	e.seedToken(t, "t-expired", wsA, destA, "raw-expired", &past, nil)
	e.seedToken(t, "t-boundary", wsA, destA, "raw-boundary", &frozen, nil)
	e.seedToken(t, "t-noexp", wsA, destA, "raw-noexp", nil, nil)
	e.seedToken(t, "t-revoked", wsA, destA, "raw-revoked", &future, &revAt)
	e.seedToken(t, "t-b", wsB, destB, "raw-b", &future, nil)
	cases := []struct {
		raw, dest, ws string
	}{
		{"raw-active", destA, wsA}, {"raw-b", destB, wsB},
		{"raw-active", destB, ""}, {"raw-active", "nope", ""}, {"raw-active", "", ""}, {"raw-active", strings.ToUpper(destA), ""},
		{"raw-expired", destA, ""}, {"raw-boundary", destA, ""}, {"raw-noexp", destA, ""}, {"raw-revoked", destA, ""},
		{"raw-b", destA, ""}, {"unknown", destA, ""}, {"", destA, ""}, {" raw-active", destA, ""},
	}
	var errTexts []string
	for _, c := range cases {
		ws, err := destinbound.Resolve(ctx, e.app, c.raw, c.dest, frozen)
		if c.ws != "" {
			if err != nil || ws != c.ws {
				t.Errorf("Resolve(%q,%q) = %q,%v; want %q", c.raw, c.dest, ws, err, c.ws)
			}
			continue
		}
		if !errors.Is(err, destinbound.ErrInvalidToken) || ws != "" {
			t.Errorf("Resolve(%q,%q) = %q,%v; want ErrInvalidToken", c.raw, c.dest, ws, err)
			continue
		}
		errTexts = append(errTexts, err.Error())
		if strings.Contains(err.Error(), c.raw) && c.raw != "" {
			t.Errorf("error leaks the token: %v", err)
		}
	}
	for _, s := range errTexts {
		if s != "invalid or revoked destination token" {
			t.Fatalf("error text %q", s)
		}
	}
	// Failed attempts never stamp last_used_at.
	var stamped []string
	rows, _ := e.owner.Query(ctx, `SELECT id FROM destination_inbound_tokens WHERE last_used_at IS NOT NULL ORDER BY id`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		stamped = append(stamped, id)
	}
	rows.Close()
	if !reflect.DeepEqual(stamped, []string{"t-active", "t-b"}) {
		t.Fatalf("stamped = %v", stamped)
	}
}

func TestResolveConstantTimeAndStampsLastUsed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	future := frozen.Add(time.Hour)
	e.seedToken(t, "t1", wsA, destA, "raw-1", &future, nil)
	var before *time.Time
	if err := e.owner.QueryRow(ctx, `SELECT last_used_at FROM destination_inbound_tokens WHERE id='t1'`).Scan(&before); err != nil || before != nil {
		t.Fatalf("precondition: %v %v", before, err)
	}
	if _, err := destinbound.Resolve(ctx, e.app, "raw-1", destA, frozen); err != nil {
		t.Fatal(err)
	}
	var got time.Time
	if err := e.owner.QueryRow(ctx, `SELECT last_used_at FROM destination_inbound_tokens WHERE id='t1'`).Scan(&got); err != nil || !got.Equal(frozen) {
		t.Fatalf("last_used_at = %v err %v", got, err)
	}
	// The comparison uses crypto/subtle on the stored digest: pin it in the source.
	src, err := os.ReadFile("token.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare") {
		t.Fatal("Resolve must compare digests with subtle.ConstantTimeCompare")
	}
	// Session TZ must not skew expiry: compare as timestamptz.
	pool2cfg := e.app.Config().Copy()
	pool2cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Kolkata"
	pool2cfg.MaxConns = 2
	pool2, err := pgxpool.NewWithConfig(ctx, pool2cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool2.Close()
	if _, err := destinbound.Resolve(ctx, pool2, "raw-1", destA, frozen.Add(30*time.Minute)); err != nil {
		t.Fatalf("valid token rejected under a non-UTC session: %v", err)
	}
	if _, err := destinbound.Resolve(ctx, pool2, "raw-1", destA, frozen.Add(61*time.Minute)); !errors.Is(err, destinbound.ErrInvalidToken) {
		t.Fatalf("expired token accepted under a non-UTC session: %v", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A tenant tx for workspace B cannot be used to act for workspace A.
	e.in(t, wsB, func(tx pgx.Tx) error {
		if _, err := destinbound.Rotate(ctx, tx, wsA, destA, 1, frozen); err == nil {
			t.Error("Rotate accepted a foreign workspace")
		}
		return nil
	})
	e.in(t, wsB, func(tx pgx.Tx) error {
		if _, err := destinbound.Status(ctx, tx, wsA, destA, frozen); err == nil {
			t.Error("Status accepted a foreign workspace")
		}
		if _, err := destinbound.Revoke(ctx, tx, wsA, destA, frozen); err == nil {
			t.Error("Revoke accepted a foreign workspace")
		}
		_, _, err := destinbound.Reconcile(ctx, tx, destinbound.Destination{ID: destA, WorkspaceID: wsA, DestinationType: "hubspot"}, mustBody(t, `{"external_event_id":"x","lead_id":1}`))
		if err == nil {
			t.Error("Reconcile accepted a foreign workspace")
		}
		return nil
	})
	var n int
	_ = e.owner.QueryRow(ctx, `SELECT count(*) FROM destination_inbound_tokens`).Scan(&n)
	if n != 0 {
		t.Fatalf("tokens created: %d", n)
	}
	// A workspace-B lead is never matched or written through workspace A's destination.
	e.lead(t, 501, wsB, map[string]any{"company": "B Co", "email": "shared@x.test", "city": ""})
	e.lead(t, 502, wsA, map[string]any{"company": "A Co", "email": "only-a@x.test", "city": ""})
	d := destinbound.Destination{ID: destA, WorkspaceID: wsA, DestinationType: "hubspot", Config: json.RawMessage(`{}`), FieldMap: json.RawMessage(`{}`)}
	for i, body := range []string{
		`{"external_event_id":"t1","lead_id":501,"fields":{"city":"Hacked"}}`,
		`{"external_event_id":"t2","fields":{"email":"shared@x.test","city":"Hacked"}}`,
	} {
		var r destinbound.Receipt
		e.in(t, wsA, func(tx pgx.Tx) (err error) { r, _, err = destinbound.Reconcile(ctx, tx, d, mustBody(t, body)); return })
		if r.Status != "unmatched" {
			t.Fatalf("case %d: status %s", i, r.Status)
		}
	}
	var city *string
	_ = e.owner.QueryRow(ctx, `SELECT city FROM leads WHERE id=501`).Scan(&city)
	if city == nil || *city != "" {
		t.Fatalf("foreign lead modified: %v", city)
	}
	// Receipts written for A are invisible to B (RLS) and carry workspace A.
	e.in(t, wsB, func(tx pgx.Tx) error {
		var c int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM destination_inbound_receipts`).Scan(&c)
		if c != 0 {
			t.Errorf("workspace B sees %d receipts", c)
		}
		return nil
	})
	// Python parity: replay of an event id is per (workspace, destination).
	d2 := destinbound.Destination{ID: destB, WorkspaceID: wsB, DestinationType: "hubspot", Config: json.RawMessage(`{}`), FieldMap: json.RawMessage(`{}`)}
	e.in(t, wsB, func(tx pgx.Tx) error {
		r, replay, err := destinbound.Reconcile(ctx, tx, d2, mustBody(t, `{"external_event_id":"t1","lead_id":501,"fields":{"city":"Own"}}`))
		if err != nil || replay || r.Status != "applied" {
			t.Fatalf("workspace B event t1: %+v replay=%v err=%v", r, replay, err)
		}
		return nil
	})
}

// ---- reconcile ----------------------------------------------------------

func mustBody(t *testing.T, s string) destinbound.InboundBody {
	t.Helper()
	b, err := destinbound.ParseInboundBody([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// lead inserts a lead as owner with Python's column defaults (” text, 0
// employees, status 'new') overridden by vals (nil -> NULL).
func (e *env) lead(t *testing.T, id int, ws string, vals map[string]any) {
	t.Helper()
	row := map[string]any{}
	for _, c := range allCols {
		row[c] = ""
	}
	row["employee_count_exact"] = 0
	row["status"] = "new"
	for k, v := range vals {
		row[k] = v
	}
	cols, ph, args := "id, workspace_id", "$1, $2", []any{id, ws}
	for _, c := range allCols {
		args = append(args, row[c])
		cols += ", " + c
		ph += fmt.Sprintf(", $%d", len(args))
	}
	mustExec(t, e.owner, `INSERT INTO leads (`+cols+`) VALUES (`+ph+`)`, args...)
}

func (e *env) leadRow(t *testing.T, id int) map[string]any {
	t.Helper()
	var raw []byte
	if err := e.owner.QueryRow(context.Background(),
		`SELECT row_to_json(t) FROM (SELECT `+strings.Join(allCols, ",")+` FROM leads WHERE id=$1) t`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func hub(policy string) destinbound.Destination {
	cfg := `{}`
	if policy != "" {
		cfg = `{"inbound_conflict_policy":"` + policy + `"}`
	}
	return destinbound.Destination{ID: destA, WorkspaceID: wsA, DestinationType: "hubspot", Config: json.RawMessage(cfg), FieldMap: json.RawMessage(`{}`)}
}

func (e *env) reconcile(t *testing.T, d destinbound.Destination, body string) (destinbound.Receipt, bool) {
	t.Helper()
	var r destinbound.Receipt
	var replay bool
	e.in(t, d.WorkspaceID, func(tx pgx.Tx) (err error) {
		r, replay, err = destinbound.Reconcile(context.Background(), tx, d, mustBody(t, body))
		return
	})
	return r, replay
}

type reconcileFixture struct {
	Fixtures struct {
		Leads      []map[string]any `json:"leads"`
		Deliveries []struct {
			ID          int     `json:"id"`
			LeadID      int     `json:"lead_id"`
			ExternalID  string  `json:"external_id"`
			Status      string  `json:"status"`
			DeliveredAt *string `json:"delivered_at"`
		} `json:"deliveries"`
	} `json:"fixtures"`
	Cases []struct {
		Name        string `json:"name"`
		Destination struct {
			Type     string          `json:"type"`
			Config   json.RawMessage `json:"config"`
			FieldMap json.RawMessage `json:"field_map"`
		} `json:"destination"`
		Steps []struct {
			Body    json.RawMessage `json:"body"`
			Raises  string          `json:"raises"`
			Replay  bool            `json:"replay"`
			Receipt *struct {
				ID               string   `json:"id"`
				Provider         string   `json:"provider"`
				ExternalEventID  string   `json:"external_event_id"`
				ExternalRecordID *string  `json:"external_record_id"`
				LeadID           *int32   `json:"lead_id"`
				Status           string   `json:"status"`
				ConflictPolicy   string   `json:"conflict_policy"`
				AppliedFields    []string `json:"applied_fields"`
				IgnoredFields    []string `json:"ignored_fields"`
				Error            *string  `json:"error"`
			} `json:"receipt"`
		} `json:"steps"`
		LeadsAfter       map[string]map[string]any `json:"leads_after"`
		ForeignLeadAfter map[string]any            `json:"foreign_lead_after"`
		ReceiptsAfter    int                       `json:"receipts_after"`
	} `json:"cases"`
}

// normLead maps SQL NULL text to "" (the recorded Python leads_after reads the
// seeded NULL notes of lead 1 as "").
func normLead(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if v == nil {
			v = ""
		}
		out[k] = v
	}
	return out
}

func TestReconcileMatchesFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/reconcile.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx reconcileFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) < 60 {
		t.Fatalf("fixture has %d cases", len(fx.Cases))
	}
	e := newEnv(t)
	ctx := context.Background()
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			for _, s := range []string{`DELETE FROM destination_inbound_receipts`, `DELETE FROM destination_deliveries`, `DELETE FROM leads WHERE workspace_id LIKE 'p10t10-%'`} {
				mustExec(t, e.owner, s)
			}
			for _, l := range fx.Fixtures.Leads {
				id := int(l["id"].(float64))
				vals := map[string]any{}
				for k, v := range l {
					if k != "id" {
						vals[k] = v
					}
				}
				e.lead(t, id, wsA, vals)
			}
			e.lead(t, 99, wsB, map[string]any{"company": "Foreign", "email": "foreign@x.test"})
			for _, d := range fx.Fixtures.Deliveries {
				mustExec(t, e.owner, `INSERT INTO destination_deliveries (id, workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts, external_id, delivered_at)
					VALUES ($1,$2,$3,$4,$5,'upsert',$6,'f',$7,1,$8,$9::timestamp)`,
					d.ID, wsA, runA, destA, d.LeadID, fmt.Sprintf("idem-%d", d.ID), d.Status, d.ExternalID, d.DeliveredAt)
			}
			mustExec(t, e.owner, `INSERT INTO destination_deliveries (id, workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts, external_id, delivered_at)
				VALUES (100,$1,$2,$3,1,'upsert','idem-100','f','success',1,'ext-other-dest','2026-08-09')`, wsA, runA, destA2)
			mustExec(t, e.owner, `UPDATE audience_destinations SET destination_type=$2, config=$3::json, field_map=$4::json WHERE id=$1`,
				destA, c.Destination.Type, string(c.Destination.Config), string(c.Destination.FieldMap))
			if c.Name == "prior_receipt_replay" { // the recorded harness pre-seeds this receipt
				mustExec(t, e.owner, `INSERT INTO destination_inbound_receipts (id, workspace_id, destination_id, provider, external_event_id, lead_id, status, conflict_policy, applied_fields, ignored_fields, payload_fingerprint, error)
					VALUES ('rcp-1',$1,$2,'hubspot','evt-55',NULL,'unmatched','fill_missing','[]'::json,'[]'::json,'f','No workspace lead matched the callback')`, wsA, destA)
			}
			dest := destinbound.Destination{ID: destA, WorkspaceID: wsA, DestinationType: c.Destination.Type,
				Config: c.Destination.Config, FieldMap: c.Destination.FieldMap}
			for i, s := range c.Steps {
				var r destinbound.Receipt
				var replay bool
				err := tenancy.InWorkspace(ctx, e.app, wsA, func(tx pgx.Tx) error {
					body, perr := destinbound.ParseInboundBody(s.Body)
					if perr != nil {
						return perr
					}
					var rerr error
					r, replay, rerr = destinbound.Reconcile(ctx, tx, dest, body)
					return rerr
				})
				if s.Raises != "" {
					if err == nil || !strings.Contains(s.Raises, err.Error()) {
						t.Fatalf("step %d: err %v, want %q", i, err, s.Raises)
					}
					continue
				}
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				w := s.Receipt
				if replay != s.Replay {
					t.Errorf("step %d: replay %v want %v", i, replay, s.Replay)
				}
				got := destinbound.Receipt{Provider: r.Provider, ExternalEventID: r.ExternalEventID, ExternalRecordID: r.ExternalRecordID,
					LeadID: r.LeadID, Status: r.Status, ConflictPolicy: r.ConflictPolicy, AppliedFields: r.AppliedFields,
					IgnoredFields: r.IgnoredFields, Error: r.Error}
				want := destinbound.Receipt{Provider: w.Provider, ExternalEventID: w.ExternalEventID, ExternalRecordID: w.ExternalRecordID,
					LeadID: w.LeadID, Status: w.Status, ConflictPolicy: w.ConflictPolicy, AppliedFields: w.AppliedFields,
					IgnoredFields: w.IgnoredFields, Error: w.Error}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("step %d receipt:\n got %+v\nwant %+v", i, got, want)
				}
				if r.DestinationID != destA || r.CreatedAt.IsZero() || (r.ID != "rcp-1" && !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(r.ID)) {
					t.Errorf("step %d receipt envelope: %+v", i, r)
				}
			}
			for id, want := range c.LeadsAfter {
				var n int
				fmt.Sscan(id, &n)
				if got := normLead(e.leadRow(t, n)); !reflect.DeepEqual(got, normLead(want)) {
					t.Errorf("lead %s after:\n got %v\nwant %v", id, got, want)
				}
			}
			if got := normLead(e.leadRow(t, 99)); !reflect.DeepEqual(got, normLead(c.ForeignLeadAfter)) {
				t.Errorf("foreign lead changed: %v", got)
			}
			var n int
			_ = e.owner.QueryRow(ctx, `SELECT count(*) FROM destination_inbound_receipts`).Scan(&n)
			if n != c.ReceiptsAfter {
				t.Errorf("receipts = %d want %d", n, c.ReceiptsAfter)
			}
		})
	}
}

func TestReconcileReplayIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.lead(t, 1, wsA, nil)
	e.lead(t, 2, wsA, map[string]any{"city": "Pune"})
	d := hub("")
	r1, replay := e.reconcile(t, d, `{"external_event_id":"ev","lead_id":1,"fields":{"city":"A"}}`)
	if replay || r1.Status != "applied" {
		t.Fatalf("first: %+v %v", r1, replay)
	}
	// Same event, different payload and lead: the stored receipt wins, nothing is re-applied.
	r2, replay := e.reconcile(t, d, `{"external_event_id":" ev ","lead_id":2,"fields":{"city":"B","notes":"n"}}`)
	if !replay || r2.ID != r1.ID || !reflect.DeepEqual(r2.AppliedFields, []string{"city"}) || *r2.LeadID != 1 {
		t.Fatalf("replay: %+v %v", r2, replay)
	}
	if got := e.leadRow(t, 1)["city"]; got != "A" {
		t.Fatalf("city %v", got)
	}
	if got := e.leadRow(t, 2)["notes"]; got != "" {
		t.Fatalf("lead 2 modified by a replay: %v", got)
	}
	var n int
	_ = e.owner.QueryRow(context.Background(), `SELECT count(*) FROM destination_inbound_receipts`).Scan(&n)
	if n != 1 {
		t.Fatalf("receipts %d", n)
	}
	// Missing event id.
	e.in(t, wsA, func(tx pgx.Tx) error {
		_, _, err := destinbound.Reconcile(context.Background(), tx, d, mustBody(t, `{"lead_id":1}`))
		if !errors.Is(err, destinbound.ErrEventIDRequired) || err.Error() != "external_event_id is required" {
			t.Errorf("err %v", err)
		}
		return nil
	})
	// Receipt JSON shape.
	b, _ := json.Marshal(r1)
	var shape map[string]any
	_ = json.Unmarshal(b, &shape)
	if len(shape) != 12 || shape["applied_fields"] == nil {
		t.Fatalf("receipt JSON: %s", b)
	}
}

func TestReconcileConcurrentSameEventOneReceipt(t *testing.T) {
	for _, matched := range []bool{true, false} {
		t.Run(fmt.Sprintf("matched=%v", matched), func(t *testing.T) {
			e := newEnv(t)
			e.lead(t, 1, wsA, map[string]any{"city": ""})
			d := hub("crm_wins")
			body := `{"external_event_id":"race","lead_id":1,"fields":{"city":"Oslo","notes":"x"}}`
			if !matched {
				body = `{"external_event_id":"race","lead_id":4242,"fields":{"city":"Oslo"}}`
			}
			const n = 16
			var wg sync.WaitGroup
			type res struct {
				r      destinbound.Receipt
				replay bool
				err    error
			}
			out := make([]res, n)
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					out[i].err = tenancy.InWorkspace(context.Background(), e.app, wsA, func(tx pgx.Tx) (err error) {
						b, _ := destinbound.ParseInboundBody([]byte(body))
						out[i].r, out[i].replay, err = destinbound.Reconcile(context.Background(), tx, d, b)
						return
					})
				}(i)
			}
			close(start)
			wg.Wait()
			winners := 0
			ids := map[string]bool{}
			for i, r := range out {
				if r.err != nil {
					t.Fatalf("goroutine %d: %v", i, r.err)
				}
				if !r.replay {
					winners++
				}
				ids[r.r.ID] = true
				if matched && !reflect.DeepEqual(r.r.AppliedFields, []string{"city", "notes"}) {
					t.Fatalf("goroutine %d got %+v", i, r.r)
				}
			}
			if winners != 1 || len(ids) != 1 {
				t.Fatalf("winners=%d distinct receipts=%d", winners, len(ids))
			}
			var c int
			_ = e.owner.QueryRow(context.Background(), `SELECT count(*) FROM destination_inbound_receipts`).Scan(&c)
			if c != 1 {
				t.Fatalf("receipt rows %d", c)
			}
			if matched {
				if row := e.leadRow(t, 1); row["city"] != "Oslo" || row["notes"] != "x" {
					t.Fatalf("lead %v", row)
				}
			}
		})
	}
}

func TestReconcileOnlyAllowedColumnsWritten(t *testing.T) {
	e := newEnv(t)
	e.lead(t, 1, wsA, nil)
	mustExec(t, e.owner, `UPDATE leads SET score=7, source='crm', email_verify='ok' WHERE id=1`)
	before := map[string]any{}
	_ = e.owner.QueryRow(context.Background(), `SELECT to_jsonb(l) FROM leads l WHERE id=1`).Scan(&before)
	body := `{"external_event_id":"cols","lead_id":1,"fields":{"id":9,"workspace_id":"p10t10-ws-b","score":99,"source":"evil","email_verify":"x","updated_at":"now","search_tsv":"x","company_size_basis":"x","company":"Renamed","city":"Z"}}`
	r, _ := e.reconcile(t, hub("crm_wins"), body)
	if !reflect.DeepEqual(r.AppliedFields, []string{"city", "company"}) ||
		!reflect.DeepEqual(r.IgnoredFields, []string{"company_size_basis", "email_verify", "id", "score", "search_tsv", "source", "updated_at", "workspace_id"}) {
		t.Fatalf("receipt %+v", r)
	}
	after := map[string]any{}
	_ = e.owner.QueryRow(context.Background(), `SELECT to_jsonb(l) FROM leads l WHERE id=1`).Scan(&after)
	for k, v := range before {
		if k == "company" || k == "city" || k == "search_tsv" {
			continue
		}
		if !reflect.DeepEqual(after[k], v) {
			t.Errorf("column %s changed: %v -> %v", k, v, after[k])
		}
	}
	if after["company"] != "Renamed" || after["city"] != "Z" || after["workspace_id"] != wsA {
		t.Fatalf("after %v", after)
	}
	// SQL-looking values and keys are inert.
	r, _ = e.reconcile(t, hub("crm_wins"), `{"external_event_id":"inj","lead_id":1,"fields":{"city":"x'; DROP TABLE leads; --","city = 'a', score":1}}`)
	if r.Status != "applied" || !reflect.DeepEqual(r.IgnoredFields, []string{"city = 'a', score"}) {
		t.Fatalf("receipt %+v", r)
	}
	if got := e.leadRow(t, 1)["city"]; got != "x'; DROP TABLE leads; --" {
		t.Fatalf("city %v", got)
	}
}

func TestFillMissingVsCRMWins(t *testing.T) {
	e := newEnv(t)
	e.lead(t, 1, wsA, map[string]any{"city": "Pune", "state": "N/A", "notes": nil, "contact_title": ""})
	fm, _ := e.reconcile(t, hub(""), `{"external_event_id":"f1","lead_id":1,"fields":{"city":"Oslo","state":"TX","notes":"n","contact_title":"CEO"}}`)
	if !reflect.DeepEqual(fm.AppliedFields, []string{"contact_title", "notes", "state"}) || !reflect.DeepEqual(fm.IgnoredFields, []string{"city"}) || fm.ConflictPolicy != "fill_missing" {
		t.Fatalf("fill_missing %+v", fm)
	}
	cw, _ := e.reconcile(t, hub("crm_wins"), `{"external_event_id":"c1","lead_id":1,"fields":{"city":"Oslo","state":"TX","blank":"","company":""}}`)
	if !reflect.DeepEqual(cw.AppliedFields, []string{"city"}) || cw.ConflictPolicy != "crm_wins" || !reflect.DeepEqual(cw.IgnoredFields, []string{"blank", "company"}) {
		t.Fatalf("crm_wins %+v", cw)
	}
	bogus, _ := e.reconcile(t, hub("bogus"), `{"external_event_id":"b1","lead_id":1,"fields":{"city":"Rome"}}`)
	if bogus.ConflictPolicy != "fill_missing" || bogus.Status != "no_change" {
		t.Fatalf("bogus %+v", bogus)
	}
}

func TestEmployeeCountCoercion(t *testing.T) {
	e := newEnv(t)
	e.lead(t, 1, wsA, map[string]any{"employee_count_exact": nil})
	d := hub("crm_wins")
	cases := []struct {
		val  string
		want any // nil -> ignored
	}{
		{`"42"`, 42}, {`42.9`, 42}, {`true`, 1}, {`" 7 "`, 7}, {`"1_0"`, 10}, {`"many"`, nil}, {`"4.2"`, nil},
		{`null`, nil}, {`[1]`, nil}, {`99999999999`, nil}, {`"99999999999"`, nil}, {`-3`, -3},
	}
	for i, c := range cases {
		mustExec(t, e.owner, `UPDATE leads SET employee_count_exact=0 WHERE id=1`)
		r, _ := e.reconcile(t, d, fmt.Sprintf(`{"external_event_id":"n%d","lead_id":1,"fields":{"employee_count_exact":%s}}`, i, c.val))
		got := e.leadRow(t, 1)["employee_count_exact"]
		if c.want == nil {
			if got != float64(0) || !reflect.DeepEqual(r.IgnoredFields, []string{"employee_count_exact"}) || len(r.AppliedFields) != 0 {
				t.Errorf("%s: want ignored, got %+v lead=%v", c.val, r, got)
			}
			continue
		}
		if got != float64(c.want.(int)) || !reflect.DeepEqual(r.AppliedFields, []string{"employee_count_exact"}) {
			t.Errorf("%s: got %+v lead=%v want %v", c.val, r, got, c.want)
		}
	}
}

func TestUnmatchedLeadReceipt(t *testing.T) {
	e := newEnv(t)
	e.lead(t, 1, wsA, nil)
	r, replay := e.reconcile(t, hub(""), `{"external_event_id":"u1","external_record_id":" rec-9 ","lead_id":777,"fields":{"city":"X","zz":1}}`)
	if replay || r.Status != "unmatched" || r.Error == nil || *r.Error != "No workspace lead matched the callback" ||
		r.LeadID == nil || *r.LeadID != 777 || r.ExternalRecordID == nil || *r.ExternalRecordID != "rec-9" ||
		len(r.AppliedFields) != 0 || !reflect.DeepEqual(r.IgnoredFields, []string{"zz"}) {
		t.Fatalf("%+v", r)
	}
	r, _ = e.reconcile(t, hub(""), `{"external_event_id":"u2","fields":{"city":"X"}}`)
	if r.Status != "unmatched" || r.LeadID != nil {
		t.Fatalf("%+v", r)
	}
}

func TestEmailMatchRequiresUnique(t *testing.T) {
	e := newEnv(t)
	e.lead(t, 1, wsA, map[string]any{"company": "D1", "email": "dup@x.test"})
	e.lead(t, 2, wsA, map[string]any{"company": "D2", "email": "dup@x.test"})
	e.lead(t, 3, wsA, map[string]any{"company": "S3", "email": "solo@x.test"})
	e.lead(t, 4, wsB, map[string]any{"company": "S4", "email": "solo-b@x.test"})
	r, _ := e.reconcile(t, hub(""), `{"external_event_id":"m1","fields":{"email":"dup@x.test","city":"Q"}}`)
	if r.Status != "unmatched" || r.LeadID != nil {
		t.Fatalf("duplicate email matched: %+v", r)
	}
	r, _ = e.reconcile(t, hub(""), `{"external_event_id":"m2","fields":{"email":"  solo@x.test ","city":"Q"}}`)
	if r.Status != "applied" || r.LeadID == nil || *r.LeadID != 3 {
		t.Fatalf("unique email: %+v", r)
	}
	r, _ = e.reconcile(t, hub(""), `{"external_event_id":"m3","fields":{"email":"solo-b@x.test","city":"Q"}}`)
	if r.Status != "unmatched" {
		t.Fatalf("foreign-workspace email matched: %+v", r)
	}
}
