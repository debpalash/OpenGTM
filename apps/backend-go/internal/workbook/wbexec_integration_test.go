//go:build integration

package workbook_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// Plan 15 WP-WBEXEC: every recorded Python scenario (testdata/wbexec_vectors.json) replayed
// through the Go run_workbook executor with GO_EXECUTOR_FULL_RUN on. The seams are the same as
// the recorder's: provider HTTP (declarative manifests on a simulator), the email verify
// cascade, the destination clients and the webhook transport.

type wbScenario struct {
	Name       string                     `json:"name"`
	Provenance bool                       `json:"provenance"`
	Automation bool                       `json:"automations"`
	Force      bool                       `json:"force"`
	Columns    json.RawMessage            `json:"columns"`
	Row        wbRow                      `json:"row"`
	Providers  map[string]wbProvider      `json:"providers"`
	Verify     json.RawMessage            `json:"verify"`
	Dest       map[string]json.RawMessage `json:"dest"`
	LeadFP     map[string]string          `json:"lead_field_provenance"`
	Prior      []map[string]any           `json:"prior_enrichments"`
	Expect     wbExpect                   `json:"expect"`
}

type wbRow struct {
	ID          int64           `json:"id"`
	LeadID      *int64          `json:"lead_id"`
	Data        json.RawMessage `json:"data"`
	Enrichments json.RawMessage `json:"enrichments"`
}

type wbProvider struct {
	Success    *bool             `json:"success"`
	Fields     map[string]string `json:"fields"`
	Confidence float64           `json:"confidence"`
}

type wbExpect struct {
	RowResult     struct{ Completed, Errors int } `json:"row_result"`
	Overlay       map[string]any                 `json:"overlay"`
	Enrichments   []map[string]any               `json:"enrichments"`
	Broadcasts    []map[string]any               `json:"broadcasts"`
	ProviderCalls []string                       `json:"provider_calls"`
	Writebacks    []struct {
		LeadID  int64          `json:"lead_id"`
		Updates map[string]any `json:"updates"`
	} `json:"writebacks"`
	VerifyCalls []map[string]any `json:"verify_calls"`
	OutputCalls []map[string]any `json:"output_calls"`
	Emitted     []struct {
		Changes []map[string]any `json:"changes"`
	} `json:"emitted"`
}

type wbVectors struct {
	Now       string       `json:"now"`
	Workspace string       `json:"workspace_id"`
	Scenarios []wbScenario `json:"scenarios"`
}

const pyRowLead = 501

// ── fakes ────────────────────────────────────────────────────────────────

type recorder struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (r *recorder) add(m map[string]any) {
	r.mu.Lock()
	r.calls = append(r.calls, m)
	r.mu.Unlock()
}

type memPub struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (p *memPub) Publish(_ context.Context, _ string, msg map[string]any) {
	b, _ := json.Marshal(msg)
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	p.mu.Lock()
	p.msgs = append(p.msgs, v)
	p.mu.Unlock()
}

type memEmitter struct {
	mu      sync.Mutex
	changes []automationhook.Change
}

func (m *memEmitter) RowsChanged(_ context.Context, _ pgx.Tx, _, _ string, ch []automationhook.Change) (int, error) {
	m.mu.Lock()
	m.changes = append(m.changes, ch...)
	m.mu.Unlock()
	return len(ch), nil
}

func answerObj(raw json.RawMessage) *wbcrud.Obj {
	o := wbcrud.NewObj()
	var m map[string]any
	if len(raw) == 0 {
		o.Set("success", true)
		return o
	}
	_ = json.Unmarshal(raw, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, m[k])
	}
	return o
}

func objMap(o *wbcrud.Obj) any {
	if o == nil {
		return nil
	}
	b, _ := json.Marshal(o)
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	return v
}

type fakePorts struct {
	rec  *recorder
	dest map[string]json.RawMessage
}

func (f fakePorts) Push(_ context.Context, ws string, cfg, lead map[string]any, wb string, leadID int64) crmpush.Result {
	f.rec.add(map[string]any{"kind": "crm", "cfg": cfg, "workspace_id": ws, "workbook_id": wb, "lead_id": leadID, "lead_email": lead["email"]})
	var a struct {
		Success bool    `json:"success"`
		Value   string  `json:"value"`
		Error   *string `json:"error"`
	}
	_ = json.Unmarshal(f.dest["crm"], &a)
	r := crmpush.Result{Success: a.Success, Value: a.Value}
	if a.Error != nil {
		r.Error = *a.Error
	}
	return r
}

type sheetsFake struct{ fakePorts }

func (f sheetsFake) AppendRow(_ context.Context, ws, id string, values []any, rng string) (*wbcrud.Obj, error) {
	f.rec.add(map[string]any{"kind": "sheets", "spreadsheet_id": id, "values": values, "range": rng, "workspace_id": ws})
	return answerObj(f.dest["sheets"]), nil
}

type airtableFake struct{ fakePorts }

func (f airtableFake) PushRecord(_ context.Context, ws string, fields *wbcrud.Obj, base, table string, typecast bool) (*wbcrud.Obj, error) {
	f.rec.add(map[string]any{"kind": "airtable", "fields": objMap(fields), "base_id": base, "table": table, "typecast": typecast, "workspace_id": ws})
	return answerObj(f.dest["airtable"]), nil
}

type instantlyFake struct{ fakePorts }

func (f instantlyFake) AddLeadToCampaign(_ context.Context, ws, c string, lead *wbcrud.Obj, skip bool) (*wbcrud.Obj, error) {
	f.rec.add(map[string]any{"kind": "instantly", "campaign_id": c, "lead": objMap(lead), "skip_if_in_campaign": skip, "workspace_id": ws})
	return answerObj(f.dest["instantly"]), nil
}

type smartleadFake struct{ fakePorts }

func (f smartleadFake) AddLeadToCampaign(_ context.Context, ws, c string, lead, settings *wbcrud.Obj) (*wbcrud.Obj, error) {
	f.rec.add(map[string]any{"kind": "smartlead", "campaign_id": c, "lead": objMap(lead), "settings": objMap(settings), "workspace_id": ws})
	return answerObj(f.dest["smartlead"]), nil
}

type outreachFake struct{ fakePorts }

func (f outreachFake) seq() map[string]bool {
	m := map[string]bool{"exists": true}
	_ = json.Unmarshal(f.dest["sequencer"], &m)
	return m
}
func (f outreachFake) SequenceExists(context.Context, string, string) (bool, error) {
	return f.seq()["exists"], nil
}
func (f outreachFake) IsSuppressed(context.Context, string, string) (bool, error) {
	return f.seq()["suppressed"], nil
}
func (f outreachFake) Enroll(_ context.Context, _, seq string, leadID int64, email, consent string) (bool, error) {
	f.rec.add(map[string]any{"kind": "sequencer", "sequence_id": seq, "lead_id": leadID, "email": email, "consent_source": consent})
	return !f.seq()["already"], nil
}

// ── harness ──────────────────────────────────────────────────────────────

func wbManifest(name string, p wbProvider) string {
	maps := map[string]string{}
	for f := range p.Fields {
		maps[f] = "$." + f
	}
	if len(maps) == 0 {
		maps["phone"] = "$.phone"
	}
	conf := p.Confidence
	if conf == 0 {
		conf = 0.5 // provider.default_confidence of the recorder's fake providers
	}
	b, _ := json.Marshal(map[string]any{"name": name, "capability": "x", "default_confidence": conf, "auth": map[string]any{"type": "none"},
		"request":  map[string]any{"method": "POST", "url": "http://sim.provider.example/" + name, "timeout": 5, "body_template": map[string]any{"company": "{{input.company}}"}},
		"response": map[string]any{"mappings": maps}})
	return string(b)
}

// twoWayClient dials :443 to the TLS webhook server and anything else to the provider simulator.
func twoWayClient(sim *httptest.Server, hooks *httptest.Server) *http.Client {
	tr := hooks.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: tr.TLSClientConfig.RootCAs}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		to := sim.Listener.Addr().String()
		if strings.HasSuffix(addr, ":443") {
			to = hooks.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, to)
	}
	return &http.Client{Transport: tr}
}

// remap rewrites Go row/lead ids to the recorder's (11/12 and 501), recursively.
type idMap map[float64]float64

func (m idMap) apply(v any) any {
	switch x := v.(type) {
	case float64:
		if r, ok := m[x]; ok {
			return r
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = m.apply(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = m.apply(e)
		}
		return out
	}
	return v
}

func norm(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, w := norm(got), norm(want)
	if g == nil {
		g = []any{}
	}
	if w == nil {
		w = []any{}
	}
	if !reflect.DeepEqual(g, w) {
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		t.Errorf("%s:\n got  %s\n want %s", what, gb, wb)
	}
}

func TestWbExecVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/wbexec_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v wbVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Scenarios) < 50 {
		t.Fatalf("vector file shrank: %d", len(v.Scenarios))
	}
	now, _ := time.Parse(time.RFC3339, v.Now)
	for _, sc := range v.Scenarios {
		t.Run(sc.Name, func(t *testing.T) { runWbScenario(t, v.Workspace, now, sc) })
	}
}

func runWbScenario(t *testing.T, ws string, now time.Time, sc wbScenario) {
	e := setup(t, ws)
	wb := "wb-wbexec" // the recorder's workbook id (setup clears the workspace per scenario)
	e.inWS(ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `DELETE FROM leads`)
		return err
	})
	ids := idMap{}
	var goLead *int64
	if sc.Row.LeadID != nil {
		var id int64
		fp := sc.LeadFP[fmt.Sprint(*sc.Row.LeadID)]
		e.inWS(ws, func(tx pgx.Tx) error {
			// An id far from the row serials keeps the row and lead remaps apart.
			return tx.QueryRow(e.ctx, `INSERT INTO leads (id, workspace_id, company, field_provenance)
			  VALUES (900000000 + (random() * 1000000)::int, $1, 'Acme', NULLIF($2, '')) RETURNING id`, ws, fp).Scan(&id)
		})
		goLead = &id
		ids[float64(id)] = pyRowLead
	}
	var goRow int64
	enr := sc.Row.Enrichments
	if len(enr) == 0 {
		enr = json.RawMessage(`{}`)
	}
	e.inWS(ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `INSERT INTO workbooks (id, name, status, workspace_id, source_type, columns_config, budget_max_usd, budget_spent_usd, total_rows, completed_rows)
		  VALUES ($1, 'wb', 'running', $2, 'manual', $3::json, 0, 0, 1, 0)`, wb, ws, string(sc.Columns)); err != nil {
			return err
		}
		if err := tx.QueryRow(e.ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments, lead_id)
		  VALUES ($1, $2, 0, $3::json, $4::json, $5) RETURNING id`, wb, ws, string(sc.Row.Data), string(enr), goLead).Scan(&goRow); err != nil {
			return err
		}
		for _, p := range sc.Prior {
			if _, err := tx.Exec(e.ctx, `INSERT INTO workbook_enrichments (workbook_id, workspace_id, lead_id, column_id, value, status, provider)
			  VALUES ($1, $2, $3, $4, $5, $6, $7)`, wb, ws, *goLead, p["column_id"], p["value"], p["status"], p["provider"]); err != nil {
				return err
			}
		}
		return nil
	})
	ids[float64(goRow)] = float64(sc.Row.ID)

	// Provider simulator: answers each provider with its scripted fields.
	sim := newSim(t, func(name string, _ int, w http.ResponseWriter, _ *http.Request) {
		p := sc.Providers[name]
		if p.Success != nil && !*p.Success {
			fmt.Fprint(w, `{}`)
			return
		}
		_ = json.NewEncoder(w).Encode(p.Fields)
	})
	rec := &recorder{}
	hooks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hdr := map[string]any{}
		for k, vs := range r.Header {
			switch strings.ToLower(k) {
			case "host", "accept", "accept-encoding", "connection", "user-agent", "content-length":
				continue
			}
			hdr[strings.ToLower(k)] = strings.Join(vs, ", ")
		}
		rec.add(map[string]any{"kind": "webhook", "method": r.Method, "url": "https://" + r.Host + r.URL.RequestURI(), "headers": hdr, "body": string(body)})
		var a struct {
			Status int    `json:"status"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(sc.Dest["webhook"], &a)
		if a.Status == 0 {
			a.Status = 200
		}
		w.WriteHeader(a.Status)
		fmt.Fprint(w, a.Text)
	}))
	t.Cleanup(hooks.Close)

	costs := map[string]float64{}
	bodies := map[string]string{}
	manifests := map[string]json.RawMessage{}
	shas := map[string]string{}
	for name, p := range sc.Providers {
		costs[name] = 0
		bodies[name] = wbManifest(name, p)
		manifests[name] = json.RawMessage(bodies[name])
		shas[name] = "deadbeef"
	}
	payload, _ := json.Marshal(map[string]any{"workspace_id": ws, "workbook_id": wb, "retry_passes": 0, "concurrency": 1, "force": sc.Force,
		"go": map[string]any{"version": 1, "manifests": manifests, "manifest_sha256": shas, "costs": costs, "default_concurrency": 1}})

	pub := &memPub{}
	var verifyCalls []map[string]any
	fp := fakePorts{rec: rec, dest: sc.Dest}
	deps := e.deps(sim, pub)
	deps.Client = twoWayClient(sim.srv, hooks)
	deps.Manifests = manifestSrv{body: bodies}
	deps.FullRun, deps.Provenance = true, sc.Provenance
	deps.Now = func() time.Time { return now }
	deps.URLCheck = func(u string) error {
		if strings.Contains(u, "blocked") {
			return errors.New("host resolves to non-public address 10.0.0.1")
		}
		return nil
	}
	deps.Verify = workbook.VerifyFunc(func(_ context.Context, w, email string) (emailverify.Result, error) {
		verifyCalls = append(verifyCalls, map[string]any{"email": email, "workspace_id": w})
		if string(sc.Verify) == `"raise"` {
			return emailverify.Result{}, errors.New("verifier down")
		}
		var r emailverify.Result
		_ = json.Unmarshal(sc.Verify, &r)
		return r, nil
	})
	deps.Output = &workbook.OutputPorts{CRM: fp, Sheets: sheetsFake{fp}, Airtable: airtableFake{fp},
		Instantly: instantlyFake{fp}, Smartlead: smartleadFake{fp}, Outreach: outreachFake{fp}}
	em := &memEmitter{}
	if sc.Automation {
		deps.Emitter = em
	}
	res, err := workbook.Run(e.ctx, deps, e.claim(ws, payload))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	x := sc.Expect
	// Deviation (fixed Python bug): a condition skip is not an error.
	skipped := 0
	for _, c := range x.Overlay {
		if m, ok := c.(map[string]any); ok && m["status"] == "skipped" {
			skipped++
		}
	}
	if res.Completed != x.RowResult.Completed || res.Errors != x.RowResult.Errors-skipped {
		t.Errorf("result: completed %d errors %d, python %+v (skipped %d)", res.Completed, res.Errors, x.RowResult, skipped)
	}

	var overlay map[string]any
	e.inWS(ws, func(tx pgx.Tx) error {
		var b []byte
		if err := tx.QueryRow(e.ctx, `SELECT enrichments FROM workbook_rows WHERE id = $1`, goRow).Scan(&b); err != nil {
			return err
		}
		return json.Unmarshal(b, &overlay)
	})
	sameJSON(t, "overlay", ids.apply(norm(overlay)), x.Overlay)

	var cells []map[string]any
	e.inWS(ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(e.ctx, `SELECT json_build_object('lead_id', lead_id, 'column_id', column_id, 'value', value, 'status', status,
		  'provider', provider, 'error', error, 'cell_metadata', cell_metadata) FROM workbook_enrichments WHERE workbook_id = $1 ORDER BY column_id`, wb)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m map[string]any
			if err := rows.Scan(&m); err != nil {
				return err
			}
			cells = append(cells, m)
		}
		return rows.Err()
	})
	sameJSON(t, "workbook_enrichments", ids.apply(norm(cells)), norm(x.Enrichments))

	var cellMsgs []any
	for _, m := range pub.msgs {
		if m["type"] == "cell_update" {
			cellMsgs = append(cellMsgs, m)
		}
	}
	sameJSON(t, "broadcasts", ids.apply(norm(cellMsgs)), norm(x.Broadcasts))

	want := map[string]int{}
	for _, n := range x.ProviderCalls {
		want[n]++
	}
	for n := range sc.Providers {
		if sim.count(n) != want[n] {
			t.Errorf("provider %s called %d times, python %d", n, sim.count(n), want[n])
		}
	}

	if len(x.Writebacks) > 0 {
		var lead map[string]any
		e.inWS(ws, func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, `SELECT row_to_json(l) FROM leads l WHERE id = $1`, *goLead).Scan(&lead)
		})
		wantFP := map[string]any{}
		for _, wbk := range x.Writebacks {
			for k, val := range wbk.Updates {
				switch k {
				case "field_provenance":
					var m map[string]any
					_ = json.Unmarshal([]byte(val.(string)), &m)
					for f, p := range m {
						wantFP[f] = p
					}
				default:
					if fmt.Sprint(lead[k]) != fmt.Sprint(val) {
						t.Errorf("lead.%s = %v, python wrote %v", k, lead[k], val)
					}
				}
			}
		}
		if len(wantFP) > 0 {
			var got map[string]any
			_ = json.Unmarshal([]byte(lead["field_provenance"].(string)), &got)
			sameJSON(t, "field_provenance", got, wantFP)
		} else if lead["field_provenance"] != nil && !sc.Provenance {
			t.Errorf("field_provenance written with provenance off: %v", lead["field_provenance"])
		}
	}

	sameJSON(t, "verify calls", verifyCalls, x.VerifyCalls)

	// Webhook bodies compare as JSON values (Go writes the row with sorted keys).
	decodeBody := func(calls []any) {
		for _, c := range calls {
			if m, ok := c.(map[string]any); ok && m["kind"] == "webhook" {
				var b any
				if s, ok := m["body"].(string); ok && json.Unmarshal([]byte(s), &b) == nil {
					m["body"] = b
				}
			}
		}
	}
	raw, _ := norm(rec.calls).([]any)
	decodeBody(raw)
	gotCalls := ids.apply(raw)
	wantCalls, _ := norm(x.OutputCalls).([]any)
	decodeBody(wantCalls)
	sameJSON(t, "output calls", gotCalls, wantCalls)

	checkEmitted(t, sc, ids, em)
}

// checkEmitted: Python's run loses every on_rows_changed delta (rows run under asyncio.gather,
// each with a copied context, and the parent flushes an empty buffer) and its `old` is always
// None (the uncommitted "running" write replaces the prior cell first). Go emits from the run
// with the real prior value; these are the documented deviations.
func checkEmitted(t *testing.T, sc wbScenario, ids idMap, em *memEmitter) {
	t.Helper()
	got := []any{}
	for _, c := range em.changes {
		got = append(got, map[string]any{"row_id": ids.apply(float64(c.RowID)), "field": c.Field, "old": c.Old, "new": c.New})
	}
	var want []any
	switch sc.Name {
	case "automations_same_task_flush":
		want = []any{map[string]any{"row_id": 11, "field": "ph", "old": "+1 old", "new": "+33123"}}
	case "automations_gather_flush_python_loses_deltas":
		want = []any{map[string]any{"row_id": 11, "field": "ph", "old": nil, "new": "+33123"}}
	default:
		want = []any{}
	}
	sameJSON(t, "on_rows_changed", got, want)
}
