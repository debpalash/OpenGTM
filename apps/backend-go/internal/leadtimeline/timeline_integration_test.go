//go:build integration

package leadtimeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadtimeline"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool, owner *pgxpool.Pool
	wsA, wsB    string
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{pool: testpg.NewAppPool(t, 4)}
	var err error
	e.owner, err = pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.owner.Close)
	e.wsA = fmt.Sprintf("t8a_%d", os.Getpid())
	e.wsB = fmt.Sprintf("t8b_%d", os.Getpid())
	clean := func() {
		ctx := context.Background()
		ws := []string{e.wsA, e.wsB}
		for _, tbl := range []string{"signals", "audience_membership_events", "destination_deliveries", "destination_runs", "outreach_sends", "outreach_sequences", "audience_destinations", "audiences", "leads"} {
			_, _ = e.owner.Exec(ctx, `DELETE FROM `+tbl+` WHERE workspace_id = ANY($1)`, ws)
		}
	}
	clean()
	t.Cleanup(clean)
	return e
}

func (e *env) ex(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.owner.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (e *env) lead(t *testing.T, ws, company string, created, updated, enriched any) int64 {
	t.Helper()
	var id int64
	err := e.owner.QueryRow(context.Background(),
		`INSERT INTO leads (company, city, workspace_id, created_at, updated_at, last_enriched_at) VALUES ($1,'x',$2,$3,$4,$5) RETURNING id`,
		company, ws, created, updated, enriched).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) build(t *testing.T, ws string, lead leadsread.Lead, limit int, before *float64, beforeID string) leadtimeline.Page {
	t.Helper()
	var p leadtimeline.Page
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		var err error
		p, err = leadtimeline.Build(context.Background(), tx, ws, lead, limit, before, beforeID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func s(v string) *string { return &v }

// seedHot mirrors contract_seed.py for lead 14 (workspace A) and lead 17's
// foreign rows (workspace B, same audience/destination shapes) and returns lead.
func (e *env) seedHot(t *testing.T) leadsread.Lead {
	t.Helper()
	fx := contract.Load(t, "timeline/timeline_hot")
	var body struct {
		Items []struct {
			ID, Kind, Title string
			Description     *string
			Status          string
			OccurredAt      float64 `json:"occurred_at"`
			Metadata        map[string]any
		}
	}
	if err := json.Unmarshal(fx.Response.Body, &body); err != nil {
		t.Fatal(err)
	}
	id := e.lead(t, e.wsA, "Hot Pursuit Labs", "2026-08-23T08:00:00", "2026-08-26T08:00:00", "2026-08-25T08:00:00")
	idB := e.lead(t, e.wsB, "Hot Pursuit Labs", "2026-08-23T08:00:00", "2026-08-26T08:00:00", nil)
	for _, w := range []string{e.wsA, e.wsB} {
		l := id
		if w == e.wsB {
			l = idB
		}
		_ = l
		e.ex(t, `INSERT INTO audiences (id, workspace_id, name, description, filters, member_count, created_at, updated_at) VALUES ($1,$2,'Hot accounts','','{}',1,now(),now())`, suf("aud", w, e), w)
		e.ex(t, `INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map, created_at, updated_at) VALUES ($1,$2,$3,'Contract Webhook','webhook',true,'{}','{}',now(),now())`, suf("dest", w, e), w, suf("aud", w, e))
		e.ex(t, `INSERT INTO destination_runs (id, workspace_id, destination_id, status, created_at) VALUES ($1,$2,$3,'completed',now())`, suf("run", w, e), w, suf("dest", w, e))
		e.ex(t, `INSERT INTO outreach_sequences (id, workspace_id, name, description, steps, status, created_at, updated_at) VALUES ($1,$2,'s','','[]','active',now(),now())`, suf("seq", w, e), w)
	}
	for _, it := range body.Items {
		parts := strings.SplitN(it.ID, ":", 2)
		for _, w := range []string{e.wsA, e.wsB} {
			lid := id
			if w == e.wsB {
				lid = idB
			}
			rid := atoi(parts[len(parts)-1])
			if w == e.wsB {
				rid += 1000000
			}
			ts := time.Unix(int64(it.OccurredAt), 0).UTC().Format("2006-01-02 15:04:05")
			switch it.Kind {
			case "signal":
				if w == e.wsB {
					continue
				}
				var title any = it.Title
				if it.Title == it.Status {
					title = ""
				}
				e.ex(t, `INSERT INTO signals (id, workspace_id, lead_id, company, signal_type, title, description, source, source_url, weight, created_at, read) VALUES ($1,$2,$3,'c',$4,$5,$6,$7,$8,$9,$10,false)`,
					parts[1], w, lid, it.Status, title, it.Description, it.Metadata["source"], it.Metadata["source_url"], int32(it.Metadata["weight"].(float64)), it.OccurredAt)
			case "audience":
				e.ex(t, `INSERT INTO audience_membership_events (id, workspace_id, audience_id, lead_id, event_type, snapshot, created_at) VALUES ($1,$2,$3,$4,$5,'{}',$6)`,
					rid, w, suf("aud", w, e), lid, it.Status, ts)
			case "activation":
				var summ, errm any
				if strings.HasPrefix(*it.Description, "HTTP") {
					errm = *it.Description
				} else {
					summ = *it.Description
				}
				var delivered any
				if it.Status == "delivered" {
					delivered = ts
				}
				e.ex(t, `INSERT INTO destination_deliveries (id, workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts, summary, error, delivered_at, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,'upsert',$6,'0',$7,1,$8,$9,$10,$11,$11)`,
					rid, w, suf("run", w, e), suf("dest", w, e), lid, "idem-"+it.ID+w, it.Status, summ, errm, delivered, ts)
			case "outreach":
				var subj any = it.Title
				if it.Title == "Outreach email" {
					subj = nil
				}
				step := int32(it.Metadata["step_number"].(float64))
				e.ex(t, `INSERT INTO outreach_sends (id, workspace_id, sequence_id, lead_id, step_number, to_email, subject, status, idempotency_key, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,'a@b.c',$6,$7,$8,$9,$9)`,
					rid, w, suf("seq", w, e), lid, step, subj, it.Status, "send-"+it.ID+w, ts+"+00")
			}
		}
	}
	return e.getLead(t, e.wsA, id)
}

func suf(p, w string, e *env) string {
	if w == e.wsA {
		return p + "-alpha-1"
	}
	return p + "-beta-1"
}

func atoi(s string) int {
	var n int
	fmt.Sscan(s, &n)
	return n
}

func (e *env) getLead(t *testing.T, ws string, id int64) leadsread.Lead {
	t.Helper()
	var l leadsread.Lead
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		got, err := leadsread.LeadsByIDs(context.Background(), tx, []int64{id})
		if err != nil {
			return err
		}
		if len(got) != 1 {
			return fmt.Errorf("lead %d not found", id)
		}
		l = got[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (e *env) json(t *testing.T, p leadtimeline.Page) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTimelineMatchesPython(t *testing.T) {
	e := setup(t)
	lead := e.seedHot(t)
	// lead:updated 1787711400 and lead:enriched 1787625000 per fixture; created from lead.
	cases := []struct {
		fx     string
		limit  int
		before *float64
		bid    string
	}{
		{"timeline/timeline_hot", 100, nil, ""},
		{"timeline/timeline_page1", 4, nil, ""},
		{"timeline/timeline_page2", 4, f64(1788000000.0), "signal:sig-a-03"},
		{"timeline/timeline_before_only", 100, nil, ""},
	}
	for _, c := range cases {
		fx := contract.Load(t, c.fx)
		q := fx.Request.Path
		limit, before, bid := c.limit, c.before, c.bid
		if _, rest, ok := strings.Cut(q, "?"); ok {
			limit, before, bid = parseQuery(t, rest)
		} else {
			limit, before, bid = 100, nil, ""
		}
		p := e.build(t, e.wsA, lead, limit, before, bid)
		contract.AssertEqual(t, contract.Resp{Status: 200, Headers: map[string]string{"content-type": "application/json"}, Body: e.json(t, p)}, fx.Response)
	}
}

func f64(v float64) *float64 { return &v }

func parseQuery(t *testing.T, raw string) (int, *float64, string) {
	limit, bid := 100, ""
	var before *float64
	for _, kv := range strings.Split(raw, "&") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "limit":
			limit = atoi(v)
		case "before":
			var f float64
			fmt.Sscan(v, &f)
			before = &f
		case "before_id":
			bid = v
		}
	}
	return limit, before, bid
}

func TestTimelineKeyOrderAndFloats(t *testing.T) {
	e := setup(t)
	lead := e.seedHot(t)
	p := e.build(t, e.wsA, lead, 1, nil, "")
	got := string(e.json(t, p))
	if !strings.HasPrefix(got, `{"items":[{"id":"signal:sig-a-08","kind":"signal","title":"news signal 8","description":"Seeded news event #8 for contract fixtures","status":"news","occurred_at":1788018000.5,"metadata":{"source":"crunchbase","source_url":"https://signals.example.com/a/8","weight":7}}],"next_before":1788018000.5,"next_before_id":"signal:sig-a-08"}`) {
		t.Fatalf("bytes: %s", got)
	}
	p = e.build(t, e.wsA, lead, 100, f64(1787472001), "")
	if g := string(e.json(t, p)); !strings.Contains(g, `"occurred_at":1787472000.0,"metadata":{}}],"next_before":null,"next_before_id":null}`) {
		t.Fatalf("tail: %s", g)
	}
}

func TestTimelinePagingStable(t *testing.T) {
	e := setup(t)
	lead := e.seedHot(t)
	all := e.build(t, e.wsA, lead, 200, nil, "")
	var seen []string
	var before *float64
	bid := ""
	for i := 0; i < 50; i++ {
		p := e.build(t, e.wsA, lead, 3, before, bid)
		for _, it := range p.Items {
			seen = append(seen, it.ID)
		}
		if p.NextBefore == nil {
			if p.NextBeforeID != nil {
				t.Fatal("next_before_id set without next_before")
			}
			break
		}
		before, bid = p.NextBefore, *p.NextBeforeID
	}
	var want []string
	for _, it := range all.Items {
		want = append(want, it.ID)
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("walk %v != all %v", seen, want)
	}
	if len(want) != 21 {
		t.Fatalf("expected 21 items got %d", len(want))
	}
}

func TestTimelineOnlyThisLeadAndTenantIsolation(t *testing.T) {
	e := setup(t)
	lead := e.seedHot(t)
	other := e.lead(t, e.wsA, "Other", nil, nil, nil)
	e.ex(t, `INSERT INTO signals (id, workspace_id, lead_id, signal_type, created_at) VALUES ('t8-other',$1,$2,'news',5)`, e.wsA, other)
	e.ex(t, `INSERT INTO signals (id, workspace_id, lead_id, signal_type, created_at) VALUES ('t8-ws-b',$1,$2,'news',6)`, e.wsB, lead.ID)
	for _, it := range e.build(t, e.wsA, lead, 200, nil, "").Items {
		if it.ID == "signal:t8-other" || it.ID == "signal:t8-ws-b" {
			t.Fatalf("leaked %s", it.ID)
		}
	}
	// workspace B sees only its own rows for ITS lead (no signals, B-only events)
	leadB := e.getLead(t, e.wsB, lead.ID+1)
	for _, it := range e.build(t, e.wsB, leadB, 200, nil, "").Items {
		if strings.HasPrefix(it.ID, "signal:") && it.ID != "signal:t8-ws-b" {
			t.Fatalf("B sees A's signal %s", it.ID)
		}
	}
}

func TestTimelineTitleCaseEvent(t *testing.T) {
	e := setup(t)
	lead := e.seedHot(t)
	var found bool
	for _, it := range e.build(t, e.wsA, lead, 200, nil, "").Items {
		if it.ID == "audience:1" && it.Title != nil && *it.Title == "Entered Hot accounts" {
			found = true
		}
	}
	if !found {
		t.Fatal("audience title not title-cased")
	}
}

func TestTimelineNullColumnsRenderNull(t *testing.T) {
	e := setup(t)
	id := e.lead(t, e.wsA, "Nulls", nil, nil, nil)
	e.ex(t, `INSERT INTO signals (id, workspace_id, lead_id, created_at) VALUES ('t8-null',$1,$2,NULL)`, e.wsA, id)
	e.ex(t, `INSERT INTO outreach_sends (workspace_id, lead_id, to_email, idempotency_key, created_at) VALUES ($1,$2,'a@b.c','t8-k',NULL)`, e.wsA, id)
	lead := e.getLead(t, e.wsA, id)
	got := string(e.json(t, e.build(t, e.wsA, lead, 100, nil, "")))
	for _, w := range []string{
		`{"id":"outreach:`,
		`"title":"Outreach email","description":"Sequence step 1","status":null,"occurred_at":0.0,"metadata":{"sequence_id":null,"step_number":null}}`,
		`{"id":"signal:t8-null","kind":"signal","title":null,"description":null,"status":null,"occurred_at":0.0,"metadata":{"source":null,"source_url":null,"weight":null}}`,
	} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %s in %s", w, got)
		}
	}
	if _, err := canonjson.Decode([]byte(got)); err != nil {
		t.Fatal(err)
	}
}
