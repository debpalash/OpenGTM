package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

// ── helpers ─────────────────────────────────────────────────────────────────

func pcRead(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// pcNorm round-trips v through JSON so Go and vector values compare structurally.
func pcNorm(t *testing.T, v any) any {
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

func pcEq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("%s\n got %s\nwant %s", what, g, w)
	}
}

// pcEvents renders events the way the vectors do (occurred_at "now" when it is
// the poll clock).
func pcEvents(t *testing.T, evs []DetectedEvent, now float64) any {
	t.Helper()
	out := []any{}
	for _, e := range evs {
		m := map[string]any{
			"natural_event_id": e.NaturalEventID, "signal_type": e.SignalType, "title": e.Title,
			"description": e.Description, "source": e.Source, "source_url": e.SourceURL,
			"weight": e.Weight, "occurred_at": e.OccurredAt, "lead_id": nil, "company": e.Company,
		}
		if e.LeadID != nil {
			m["lead_id"] = *e.LeadID
		}
		if e.OccurredAt == now {
			m["occurred_at"] = "now"
		}
		out = append(out, m)
	}
	return pcNorm(t, out)
}

func pcMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func pcWatchView(m map[string]any) WatchView {
	w := WatchView{ID: "w-1", Target: "Acme"}
	if m == nil {
		return w
	}
	if tg, ok := m["target"].(string); ok {
		w.Target = tg
	}
	w.ResolvedCIK, _ = m["resolved_cik"].(string)
	w.Cursor, _ = m["cursor"].(map[string]any)
	return w
}

// pcISOWeekTime is a Wednesday inside the named ISO week ("2026-W35").
func pcISOWeekTime(t *testing.T, label string) time.Time {
	t.Helper()
	var y, w int
	if _, err := fmt.Sscanf(label, "%d-W%d", &y, &w); err != nil {
		t.Fatal(err)
	}
	jan4 := time.Date(y, 1, 4, 12, 0, 0, 0, time.UTC)
	monday1 := jan4.AddDate(0, 0, -int((jan4.Weekday()+6)%7))
	got := monday1.AddDate(0, 0, (w-1)*7+2)
	if isoWeek(got) != label {
		t.Fatalf("pcISOWeekTime(%s) = %s", label, isoWeek(got))
	}
	return got
}

// pcFakePy is a scripted PyCaller.
type pcFakePy struct {
	mu    sync.Mutex
	calls []pyworker.ProviderCall
	fn    func(n int, c pyworker.ProviderCall) (pyworker.ProviderResult, error)
}

func (f *pcFakePy) Call(_ context.Context, c pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	return f.fn(n, c)
}

func pcOK(fields map[string]any) pyworker.ProviderResult {
	out, _ := json.Marshal(map[string]any{"provider": "x", "success": true, "fields": fields})
	return pyworker.ProviderResult{Status: "ok", Output: out}
}

func pcSearch(rows ...map[string]any) pyworker.ProviderResult {
	rs := make([]any, 0, len(rows))
	for _, r := range rows {
		rs = append(rs, r)
	}
	return pcOK(map[string]any{"results": rs})
}

func pcDeps(c PyCaller) PyDeps {
	return PyDeps{
		SourceDeps: SourceDeps{Deps: Deps{Now: func() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) }}},
		Py:         PyCollectors{Client: c},
	}
}

func pcWatch() PyWatch {
	return PyWatch{WatchView: WatchView{ID: "w-1", Target: "Acme Corp"}, WorkspaceID: "ws-1", FireKey: "fk-1"}
}

// ── hiring + tech ───────────────────────────────────────────────────────────

func TestHiringBandVectors(t *testing.T) {
	var v struct {
		Vectors []struct {
			Detector   string         `json:"detector"`
			Name       string         `json:"name"`
			Watch      map[string]any `json:"watch"`
			Signals    map[string]any `json:"signals"`
			WantHiring bool           `json:"want_hiring"`
			WantTech   bool           `json:"want_tech"`
			Backfill   bool           `json:"backfill"`
			ISOWeek    string         `json:"iso_week"`
			Events     []any          `json:"events"`
			Patch      map[string]any `json:"patch"`
		} `json:"vectors"`
	}
	pcRead(t, "testdata/detector_vectors.json", &v)
	n := 0
	for _, c := range v.Vectors {
		if c.Detector != "hiring_tech" {
			continue
		}
		n++
		now := pcISOWeekTime(t, c.ISOWeek)
		epoch := float64(now.UnixNano()) / 1e9
		ev, patch := detectHiringTech(pcWatchView(c.Watch), c.Signals, c.WantHiring, c.WantTech, c.Backfill, now)
		want := c.Events
		if want == nil {
			want = []any{}
		}
		pcEq(t, c.Name+" events", pcEvents(t, ev, epoch), pcNorm(t, want))
		pcEq(t, c.Name+" patch", pcNorm(t, patch), pcNorm(t, c.Patch))
	}
	if n < 25 {
		t.Fatalf("only %d hiring_tech vectors", n)
	}
}

func TestBandLadderAndISOWeek(t *testing.T) {
	for i, b := range []string{"none", "hiring", "growing", "rapid_growth", "hypergrowth"} {
		if bandRank(b) != i {
			t.Errorf("bandRank(%q) = %d, want %d", b, bandRank(b), i)
		}
	}
	for _, v := range []any{"mystery", "", nil, 5, true, []any{}} {
		if bandRank(v) != 0 {
			t.Errorf("bandRank(%v) = %d, want 0", v, bandRank(v))
		}
	}
	// datetime.isocalendar() over a year boundary (Python-verified labels).
	for ts, want := range map[string]string{
		"2026-12-31T23:59:59Z": "2026-W53", "2027-01-01T00:00:00Z": "2026-W53", "2027-01-04T00:00:00Z": "2027-W01",
		"2024-12-30T00:00:00Z": "2025-W01", "2026-08-26T12:00:00Z": "2026-W35",
	} {
		tm, _ := time.Parse(time.RFC3339, ts)
		if got := isoWeek(tm); got != want {
			t.Errorf("isoWeek(%s) = %s, want %s", ts, got, want)
		}
	}
	// A non-UTC clock is read in UTC.
	if got := isoWeek(time.Date(2027, 1, 3, 23, 0, 0, 0, time.FixedZone("x", -5*3600))); got != "2027-W01" {
		t.Errorf("isoWeek(non-UTC) = %s", got)
	}
}

type pcTranscript struct {
	Scenario   string `json:"scenario"`
	Request    map[string]any
	HTTPStatus int            `json:"http_status"`
	Response   map[string]any `json:"response"`
}

func pcLoadTranscripts(t *testing.T) map[string]pcTranscript {
	t.Helper()
	files, _ := filepath.Glob("testdata/transcripts/pyworker/*.json")
	if len(files) < 12 {
		t.Fatalf("pyworker transcripts missing: %d", len(files))
	}
	out := map[string]pcTranscript{}
	for _, f := range files {
		var raw struct {
			Scenario   string         `json:"scenario"`
			Request    map[string]any `json:"request"`
			HTTPStatus int            `json:"http_status"`
			Response   map[string]any `json:"response"`
		}
		pcRead(t, f, &raw)
		out[raw.Scenario] = pcTranscript{Scenario: raw.Scenario, Request: raw.Request, HTTPStatus: raw.HTTPStatus, Response: raw.Response}
	}
	return out
}

// pcReplayServer answers /internal/provider-call with the recorded response and
// hands every request body to check.
func pcReplayServer(t *testing.T, tr pcTranscript, check func(body map[string]any)) (*httptest.Server, *int) {
	t.Helper()
	calls := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		if check != nil {
			check(body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(tr.HTTPStatus)
		_ = json.NewEncoder(w).Encode(tr.Response)
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestHiringTechTranscripts(t *testing.T) {
	trs := pcLoadTranscripts(t)
	type want struct {
		maxJobs int
		band    string
		err     func(error) bool
	}
	cases := map[string]want{
		"hiring_hypergrowth":      {5, "hypergrowth", nil},
		"hiring_growing":          {1, "growing", nil},
		"hiring_default_max_jobs": {5, "hypergrowth", nil},
		"hiring_no_jobs":          {5, "none", nil},
		"hiring_timeout":          {5, "", func(e error) bool { return errors.Is(e, ErrPyTimeout) }},
		"hiring_worker_expired": {5, "", func(e error) bool {
			var pe *PyCallError
			return errors.As(e, &pe) && pe.Msg == "provider worker expired"
		}},
		"hiring_no_company": {5, "", func(e error) bool {
			var pe *PyCallError
			return errors.As(e, &pe) && pe.Msg == "no_company_name"
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tr := trs[name]
			srv, calls := pcReplayServer(t, tr, func(body map[string]any) {
				if body["provider"] != "jobspy_signals" || body["workspace_id"] != "ws-1" {
					t.Errorf("envelope = %v", body)
				}
				if body["call_id"] != "watch:w-1:fk-1:hiring:0" {
					t.Errorf("call_id = %v", body["call_id"])
				}
				in := pcMap(body["input"])
				if in["company"] != "Acme" || len(in) != 2 || int(in["max_jobs"].(float64)) != c.maxJobs {
					t.Errorf("input = %v", in)
				}
			})
			d := pcDeps(pyworker.New(srv.URL, "tok", srv.Client()))
			d.Py.JobSpyMaxJobs = c.maxJobs
			w := pcWatch()
			w.Target = "  Acme "
			w.Cursor = map[string]any{"bootstrapped": true, "hiring_band": "none"}
			ev, patch, err := FetchHiringAndTech(context.Background(), d, w, true, true, false)
			if c.err != nil {
				if err == nil || !c.err(err) || ev != nil || patch != nil {
					t.Fatalf("got (%v, %v, %v)", ev, patch, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 1 {
				t.Errorf("calls = %d", *calls)
			}
			if patch["hiring_band"] != c.band {
				t.Errorf("band = %v, want %s", patch["hiring_band"], c.band)
			}
			surge := 0
			tech := 0
			for _, e := range ev {
				switch e.SignalType {
				case "hiring_surge":
					surge++
				case "new_tech_adopted":
					tech++
				}
			}
			wantSurge := 0
			if c.band != "none" {
				wantSurge = 1
			}
			if surge != wantSurge {
				t.Errorf("hiring_surge events = %d, want %d", surge, wantSurge)
			}
			// The tech diff is exactly the recorded tech list.
			var sig map[string]any
			if f, _ := pcMap(pcMap(tr.Response["output"])["fields"])["hiring_signals"].(string); f != "" {
				_ = json.Unmarshal([]byte(f), &sig)
			}
			seen := map[string]bool{}
			techList, _ := sig["tech_adoption_signal"].([]any)
			for _, s := range techList {
				seen[NormalizeTech(pcMap(s)["tech"].(string))] = true
			}
			if len(seen) == 0 && name != "hiring_no_jobs" {
				t.Fatal("pcTranscript has no tech signals")
			}
			if tech != len(seen) || len(pcMap(patch["known_tech"])) != len(seen) {
				t.Errorf("tech events = %d, known_tech = %d, want %d", tech, len(pcMap(patch["known_tech"])), len(seen))
			}
		})
	}
}

func TestHiringTargetWithoutCompanyNameMakesNoCall(t *testing.T) {
	f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		t.Error("a CIK-only target must not be searched")
		return pyworker.ProviderResult{}, nil
	}}
	for _, target := range []string{"sec_cik:320193", "  SEC_CIK:5", "", "   "} {
		w := pcWatch()
		w.Target = target
		_, _, err := FetchHiringAndTech(context.Background(), pcDeps(f), w, true, true, false)
		if !errors.Is(err, ErrNoCompanyName) {
			t.Errorf("target %q: err = %v", target, err)
		}
	}
}

func TestHiringBadPayloadsAreSourceFailures(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"invalid json": {"hiring_signals": "{bad"},
		"not object":   {"hiring_signals": "[1]"},
	} {
		f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) { return pcOK(fields), nil }}
		ev, patch, err := FetchHiringAndTech(context.Background(), pcDeps(f), pcWatch(), true, true, false)
		var pe *PyCallError
		if !errors.As(err, &pe) || ev != nil || patch != nil {
			t.Errorf("%s: (%v, %v, %v)", name, ev, patch, err)
		}
	}
	// A missing hiring_signals field is the valid no-op.
	f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) { return pcOK(map[string]any{}), nil }}
	ev, patch, err := FetchHiringAndTech(context.Background(), pcDeps(f), pcWatch(), true, false, false)
	if err != nil || len(ev) != 0 || patch["hiring_band"] != "none" {
		t.Errorf("missing field: (%v, %v, %v)", ev, patch, err)
	}
}

// ── job change: normalisers ─────────────────────────────────────────────────

type pcExtra struct {
	Lower []struct{ In, Out string } `json:"lower"`
	Norm  []struct{ In, Out string } `json:"normalize_company"`
	LI    []struct{ In, Out string } `json:"linkedin_handle"`
	Co    []struct {
		Title, Snippet, Name string
		Result               []any
	} `json:"company_from_result"`
	FP []struct{ HTML, Fingerprint string } `json:"fingerprint_html"`
}

func pcLoadExtra(t *testing.T) pcExtra {
	var e pcExtra
	pcRead(t, "testdata/pycollectors_extra_vectors.json", &e)
	if len(e.Lower) < 19 || len(e.Co) < 29 || len(e.FP) < 22 {
		t.Fatalf("extra vectors truncated: %d/%d/%d", len(e.Lower), len(e.Co), len(e.FP))
	}
	return e
}

func TestJobChangeNormalizersVectors(t *testing.T) {
	var v struct {
		Norm []struct{ In, Out string } `json:"normalize_company"`
		LI   []struct {
			In  string `json:"in"`
			Out string `json:"out"`
		} `json:"linkedin_handle"`
		Key []struct {
			Contact map[string]any `json:"contact"`
			Key     string         `json:"key"`
		} `json:"contact_key"`
		Query []struct {
			Name        string `json:"name"`
			LinkedInURL string `json:"linkedin_url"`
			Company     string `json:"company"`
			Query       string `json:"query"`
		} `json:"build_query"`
	}
	pcRead(t, "testdata/job_change_vectors.json", &v)
	if len(v.Norm) != 48 || len(v.LI) != 14 || len(v.Key) != 12 || len(v.Query) != 7 {
		t.Fatalf("vector counts: %d %d %d %d", len(v.Norm), len(v.LI), len(v.Key), len(v.Query))
	}
	for _, c := range v.Norm {
		if got := NormalizeCompany(c.In); got != c.Out {
			t.Errorf("NormalizeCompany(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.LI {
		if got := LinkedInHandle(c.In); got != c.Out {
			t.Errorf("LinkedInHandle(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Key {
		ct := Contact{}
		ct.Name, _ = c.Contact["name"].(string)
		ct.Company, _ = c.Contact["company"].(string)
		ct.LinkedInURL, _ = c.Contact["linkedin_url"].(string)
		if id, ok := pcInt(c.Contact["lead_id"]); ok {
			ct.LeadID = &id
		}
		if got := ContactKey(ct); got != c.Key {
			t.Errorf("ContactKey(%v) = %q, want %q", c.Contact, got, c.Key)
		}
	}
	for _, q := range v.Query {
		if got := BuildQuery(q.Name, q.LinkedInURL, q.Company); got != q.Query {
			t.Errorf("BuildQuery(%v) = %q, want %q", q, got, q.Query)
		}
	}

	// Unicode edges where RE2 and Python's `re` differ, all recorded from Python.
	e := pcLoadExtra(t)
	for _, c := range e.Lower {
		if got := pcLower(c.In); got != c.Out {
			t.Errorf("pcLower(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range e.Norm {
		if got := NormalizeCompany(c.In); got != c.Out {
			t.Errorf("NormalizeCompany(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range e.LI {
		if got := LinkedInHandle(c.In); got != c.Out {
			t.Errorf("LinkedInHandle(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

// ── job change: company extraction ──────────────────────────────────────────

func pcRows(v any) []SearchResult {
	rows, _ := v.([]any)
	out := []SearchResult{}
	for _, r := range rows {
		m := pcMap(r)
		s := SearchResult{}
		for _, k := range []string{"href", "url"} {
			if x, _ := m[k].(string); x != "" {
				s.URL = x
				break
			}
		}
		s.Title, _ = m["title"].(string)
		for _, k := range []string{"body", "snippet"} {
			if x, _ := m[k].(string); x != "" {
				s.Snippet = x
				break
			}
		}
		out = append(out, s)
	}
	return out
}

func TestCompanyExtractionVectors(t *testing.T) {
	var v struct {
		CFR []struct {
			Title, Snippet, Name string
			Result               []any
		} `json:"company_from_result"`
		Resolve []struct {
			Name        string         `json:"name"`
			Person      string         `json:"person"`
			LinkedInURL string         `json:"linkedin_url"`
			Hint        string         `json:"company_hint"`
			Results     any            `json:"results"`
			Queries     []string       `json:"queries"`
			Result      []any          `json:"result"`
			Raised      map[string]any `json:"raised"`
		} `json:"resolve_current_company"`
	}
	pcRead(t, "testdata/job_change_vectors.json", &v)
	if len(v.CFR) != 27 || len(v.Resolve) != 14 {
		t.Fatalf("vector counts: %d %d", len(v.CFR), len(v.Resolve))
	}
	check := func(title, snippet, name string, want []any) {
		co, ok, matched := CompanyFromResult(title, snippet, name)
		if want[0] == nil {
			if ok {
				t.Errorf("CompanyFromResult(%q, %q, %q) = %q, want none", title, snippet, name, co)
			}
		} else if !ok || co != want[0] {
			t.Errorf("CompanyFromResult(%q, %q, %q) = (%q, %v), want %v", title, snippet, name, co, ok, want[0])
		}
		if matched != want[1] {
			t.Errorf("CompanyFromResult(%q, %q, %q) matched = %v, want %v", title, snippet, name, matched, want[1])
		}
	}
	for _, c := range v.CFR {
		check(c.Title, c.Snippet, c.Name, c.Result)
	}
	for _, c := range pcLoadExtra(t).Co {
		check(c.Title, c.Snippet, c.Name, c.Result)
	}
	for _, c := range v.Resolve {
		var queries []string
		search := func(_ context.Context, q string) ([]SearchResult, error) {
			queries = append(queries, q)
			if c.Raised != nil {
				return nil, errors.New("ddg 429")
			}
			return pcRows(c.Results), nil
		}
		co, ev, found, err := ResolveCurrentCompany(context.Background(), c.Person, c.LinkedInURL, c.Hint, search)
		if (err != nil) != (c.Raised != nil) {
			t.Errorf("%s: err = %v, raised = %v", c.Name, err, c.Raised)
			continue
		}
		pcEq(t, c.Name+" queries", pcNorm(t, append([]string{}, queries...)), pcNorm(t, append([]string{}, c.Queries...)))
		if c.Raised != nil {
			continue
		}
		if c.Result[0] == nil {
			if found {
				t.Errorf("%s: found %q, want none", c.Name, co)
			}
		} else if !found || co != c.Result[0] || ev != c.Result[1] {
			t.Errorf("%s: (%q, %q, %v), want %v", c.Name, co, ev, found, c.Result)
		}
	}
}

// ── job change: cap, state key, ordering ────────────────────────────────────

func TestJobChangeCapAndStateKey(t *testing.T) {
	var v struct {
		Cap []struct {
			GlobalCap int            `json:"global_cap"`
			Config    map[string]any `json:"config"`
			Cap       int            `json:"cap"`
		} `json:"per_poll_cap"`
		Fetch []struct {
			Name     string `json:"name"`
			Contacts []struct {
				Name        string  `json:"name"`
				Company     string  `json:"company"`
				LeadID      *int64  `json:"lead_id"`
				LinkedInURL string  `json:"linkedin_url"`
				Key         string  `json:"key"`
				_           float64 // keep keyed literals
			} `json:"contacts"`
			Cursor   map[string]any `json:"cursor"`
			Scripts  map[string]any `json:"scripts"`
			Config   map[string]any `json:"config"`
			Now      float64        `json:"now"`
			Queries  []string       `json:"queries"`
			Events   []any          `json:"events"`
			Patch    map[string]any `json:"patch"`
			Backfill bool           `json:"backfill"`
		} `json:"fetch_job_changes"`
	}
	pcRead(t, "testdata/job_change_vectors.json", &v)
	if len(v.Cap) != 32 || len(v.Fetch) != 18 {
		t.Fatalf("vector counts: %d %d", len(v.Cap), len(v.Fetch))
	}
	for _, c := range v.Cap {
		if got := PerPollCap(c.GlobalCap, c.Config); got != c.Cap {
			t.Errorf("PerPollCap(%d, %v) = %d, want %d", c.GlobalCap, c.Config, got, c.Cap)
		}
	}
	for _, c := range v.Fetch {
		var contacts []Contact
		byName := map[string]string{}
		for _, ct := range c.Contacts {
			contacts = append(contacts, Contact{Name: ct.Name, Company: ct.Company, LeadID: ct.LeadID, LinkedInURL: ct.LinkedInURL, Key: ct.Key})
			byName[ct.Name] = ct.Name
		}
		var queries []string
		search := func(_ context.Context, q string) ([]SearchResult, error) {
			queries = append(queries, q)
			name := strings.SplitN(strings.TrimPrefix(q, `"`), `"`, 2)[0]
			switch s := c.Scripts[name].(type) {
			case string:
				return nil, errors.New(s)
			default:
				return pcRows(s), nil
			}
		}
		now := time.Unix(0, int64(c.Now*1e9))
		ev, patch, err := fetchJobChangesWith(context.Background(), c.Cursor, c.Config, 50, contacts, now, search)
		pcEq(t, c.Name+" queries", pcNorm(t, append([]string{}, queries...)), pcNorm(t, append([]string{}, c.Queries...)))
		if c.Patch == nil {
			if err == nil || !errors.Is(err, ErrAllLookupsFailed) || ev != nil || patch != nil {
				t.Errorf("%s: total failure expected, got (%v, %v, %v)", c.Name, ev, patch, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		pcEq(t, c.Name+" events", pcEvents(t, ev, c.Now), pcNorm(t, c.Events))
		pcEq(t, c.Name+" patch", pcNorm(t, patch), pcNorm(t, c.Patch))
	}
}

func TestJobChangeCapAppliesToRPCCalls(t *testing.T) {
	mk := func(n int) []Contact {
		out := make([]Contact, n)
		for i := range out {
			out[i] = Contact{Name: fmt.Sprintf("Person %03d", i), Company: "Co"}
			out[i].Key = ContactKey(out[i])
		}
		return out
	}
	f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) { return pcSearch(), nil }}
	d := pcDeps(f)
	d.Py.JobChangeMaxContactsPerPoll = 50
	w := pcWatch()
	_, patch, err := FetchJobChanges(context.Background(), d, w, mk(120), false)
	if err != nil || len(f.calls) != 50 {
		t.Fatalf("calls = %d, err = %v", len(f.calls), err)
	}
	if n := len(pcMap(patch[JobChangeStateKey])); n != 50 {
		t.Errorf("state entries = %d, want 50 (only attempted contacts get state)", n)
	}
	// The per-watch override can only lower the cap.
	f.calls = nil
	w.Config = map[string]any{"max_contacts_per_poll": float64(7)}
	if _, _, err := FetchJobChanges(context.Background(), d, w, mk(120), false); err != nil || len(f.calls) != 7 {
		t.Fatalf("calls = %d, err = %v", len(f.calls), err)
	}
	f.calls = nil
	w.Config = map[string]any{"max_contacts_per_poll": float64(5000)}
	if _, _, err := FetchJobChanges(context.Background(), d, w, mk(120), false); err != nil || len(f.calls) != 50 {
		t.Fatalf("calls = %d, err = %v", len(f.calls), err)
	}
	// Round robin: the next poll starts with the unchecked ones.
	f.calls = nil
	w.Config = nil
	w.Cursor = patch
	if _, _, err := FetchJobChanges(context.Background(), d, w, mk(120), false); err != nil {
		t.Fatal(err)
	}
	first := string(f.calls[0].Input)
	if strings.Contains(first, "Person 000") || strings.Contains(first, "Person 049") {
		t.Errorf("round robin repeated an already checked contact: %s", first)
	}
}

func TestJobChangeDescriptionIsPythonJSON(t *testing.T) {
	lead := int64(7)
	got := jobChangeDescription(Contact{LeadID: &lead, Name: "Zoë <A&B> \"q\"\n"}, "Old ", "Nëw", "https://x/y")
	want := `{"lead_id": 7, "person_name": "Zo\u00eb <A&B> \"q\"\n", "old_company": "Old\u2028", "new_company": "N\u00ebw", "evidence_url": "https://x/y"}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if g := jobChangeDescription(Contact{}, "a", "b", ""); !strings.HasPrefix(g, `{"lead_id": null, `) {
		t.Errorf("nil lead: %s", g)
	}
}

// ── RPC behaviour ───────────────────────────────────────────────────────────

func TestDDGSTranscripts(t *testing.T) {
	trs := pcLoadTranscripts(t)
	contacts := []Contact{{Name: "Ada Lovelace", Company: "Engines", Key: "lead:14"}}
	lead := int64(14)
	contacts[0].LeadID = &lead
	cursor := map[string]any{JobChangeStateKey: map[string]any{"lead:14": map[string]any{"last_known_company": "Engines Ltd", "last_checked_at": float64(1)}}}

	run := func(name string) ([]DetectedEvent, map[string]any, error, string) {
		var query string
		srv, _ := pcReplayServer(t, trs[name], func(body map[string]any) {
			in := pcMap(body["input"])
			query, _ = in["query"].(string)
			if body["provider"] != "ddgs_search" || len(in) != 2 || in["max_results"] != float64(8) {
				t.Errorf("request = %v", body)
			}
			if body["call_id"] != "watch:w-1:fk-1:job_change:0" {
				t.Errorf("call_id = %v", body["call_id"])
			}
		})
		w := pcWatch()
		w.Cursor = cursor
		ev, patch, err := FetchJobChanges(context.Background(), pcDeps(pyworker.New(srv.URL, "tok", srv.Client())), w, contacts, false)
		return ev, patch, err, query
	}

	ev, patch, err, q := run("ddgs_rows")
	if err != nil || len(ev) != 1 {
		t.Fatalf("ddgs_rows: (%v, %v, %v)", ev, patch, err)
	}
	if q != `"Ada Lovelace" Engines site:linkedin.com/in` {
		t.Errorf("query = %q", q)
	}
	// The recorded row is "Ada Lovelace - CEO - Analytical Engines" at /in/ada: no pinned handle, name matches.
	if ev[0].Company != "Analytical Engines" || ev[0].SourceURL != "https://www.linkedin.com/in/ada" || *ev[0].LeadID != 14 {
		t.Errorf("event = %+v", ev[0])
	}

	if ev, patch, err, _ = run("ddgs_empty"); err != nil || len(ev) != 0 {
		t.Errorf("ddgs_empty: (%v, %v, %v)", ev, patch, err)
	}
	for _, name := range []string{"ddgs_transport_error", "ddgs_timeout", "ddgs_query_required"} {
		ev, patch, err, _ = run(name)
		if !errors.Is(err, ErrAllLookupsFailed) || ev != nil || patch != nil {
			t.Errorf("%s: (%v, %v, %v)", name, ev, patch, err)
		}
	}
	if _, _, err, _ = run("ddgs_timeout"); !errors.Is(err, ErrPyTimeout) {
		t.Errorf("ddgs_timeout: %v", err)
	}
}

func TestRPCCallIDsDeterministic(t *testing.T) {
	ids := func(fire string) []string {
		f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) { return pcSearch(), nil }}
		w := pcWatch()
		w.FireKey = fire
		cs := []Contact{{Name: "A B", Key: "k1"}, {Name: "C D", Key: "k2"}, {Name: "E F", Key: "k3"}}
		if _, _, err := FetchJobChanges(context.Background(), pcDeps(f), w, cs, false); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range f.calls {
			out = append(out, c.CallID)
		}
		return out
	}
	a, b := ids("watch_poll:abc"), ids("watch_poll:abc")
	want := []string{"watch:w-1:watch_poll:abc:job_change:0", "watch:w-1:watch_poll:abc:job_change:1", "watch:w-1:watch_poll:abc:job_change:2"}
	pcEq(t, "ids", pcNorm(t, a), pcNorm(t, want))
	pcEq(t, "retry ids", pcNorm(t, b), pcNorm(t, want))
	if reflect.DeepEqual(ids("watch_poll:other"), a) {
		t.Error("a different fire key must give different call ids")
	}

	// An id over the Python length limit is replaced by a digest, still deterministic.
	long := PyWatch{WatchView: WatchView{ID: strings.Repeat("w", 150)}, FireKey: strings.Repeat("f", 150)}
	x, y := pcCallID(long, "job_change", 3), pcCallID(long, "job_change", 3)
	if x != y || len(x) > 200 || pcCallID(long, "job_change", 4) == x {
		t.Errorf("long id %q (%d)", x, len(x))
	}
	// Hiring and account group use their own sources.
	f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) { return pcOK(map[string]any{}), nil }}
	w := pcWatch()
	if _, _, err := FetchHiringAndTech(context.Background(), pcDeps(f), w, true, true, false); err != nil {
		t.Fatal(err)
	}
	w.Config = map[string]any{"accounts": []any{map[string]any{"account_id": "a", "company": "C"}}}
	w.SignalTypes = []string{"partnership_hiring"}
	if _, _, err := FetchAccountGroup(context.Background(), pcDeps(f), w, false); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].CallID != "watch:w-1:fk-1:hiring:0" || f.calls[1].CallID != "watch:w-1:fk-1:account_group_hiring_0:0" {
		t.Errorf("ids = %q, %q", f.calls[0].CallID, f.calls[1].CallID)
	}
}

func TestPyworkerDownIsSourceFailure(t *testing.T) {
	d := pcDeps(nil) // PY_WORKER_URL unset: pyworker.ErrUnavailable
	w := pcWatch()
	w.Cursor = map[string]any{"bootstrapped": true}
	if ev, patch, err := FetchHiringAndTech(context.Background(), d, w, true, true, false); !errors.Is(err, pyworker.ErrUnavailable) || ev != nil || patch != nil {
		t.Errorf("hiring: (%v, %v, %v)", ev, patch, err)
	}
	cs := []Contact{{Name: "A B", Key: "k1"}, {Name: "C D", Key: "k2"}}
	if ev, patch, err := FetchJobChanges(context.Background(), d, w, cs, false); !errors.Is(err, ErrAllLookupsFailed) || !errors.Is(err, pyworker.ErrUnavailable) || ev != nil || patch != nil {
		t.Errorf("job change: (%v, %v, %v)", ev, patch, err)
	}
	// A rejected token or an unknown provider is the same kind of failure.
	for _, e := range []error{pyworker.ErrUnauthorized, pyworker.ErrUnknownProvider} {
		f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
			return pyworker.ProviderResult{Status: "error", HTTPStatus: 404, Err: e}, nil
		}}
		if _, _, err := FetchHiringAndTech(context.Background(), pcDeps(f), w, true, true, false); !errors.Is(err, e) {
			t.Errorf("%v: err = %v", e, err)
		}
		f.calls = nil
		if _, _, err := FetchJobChanges(context.Background(), pcDeps(f), w, cs, false); !errors.Is(err, e) || len(f.calls) != 1 {
			t.Errorf("%v: err = %v, calls = %d (the batch stops at the first rejection)", e, err, len(f.calls))
		}
	}
	// The account group records it as that collector's failure and carries on.
	w.Config = map[string]any{"accounts": []any{map[string]any{"account_id": "a1", "company": "C"}}}
	w.SignalTypes = []string{"partnership_hiring"}
	w.Cursor = nil
	ev, patch, err := FetchAccountGroup(context.Background(), d, w, false)
	if err != nil || len(ev) != 0 {
		t.Fatalf("account group: (%v, %v, %v)", ev, patch, err)
	}
	pcEq(t, "failures", pcNorm(t, patch["_collector_failures"]), pcNorm(t, []any{"hiring_provider_unavailable"}))
	pcEq(t, "health", pcNorm(t, patch["collector_health"]),
		pcNorm(t, map[string]any{"a1:partnership_hiring": map[string]any{"state": "failed", "attempt_count": 1, "last_error_class": "hiring_provider_unavailable"}}))
}

func TestPyworkerTimeoutBounded(t *testing.T) {
	timeoutRes := pyworker.ProviderResult{Status: "timeout", Error: "provider timed out"}
	cs := []Contact{{Name: "A A", Key: "k1"}, {Name: "B B", Key: "k2"}, {Name: "C C", Key: "k3"}, {Name: "D D", Key: "k4"}}

	// Every lookup times out: the batch stops after the first one (a hung worker
	// costs one SearchTimeout, not one per contact) and the poll is a failure.
	f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) { return timeoutRes, nil }}
	d := pcDeps(f)
	ev, patch, err := FetchJobChanges(context.Background(), d, pcWatch(), cs, false)
	if !errors.Is(err, ErrAllLookupsFailed) || !errors.Is(err, ErrPyTimeout) || ev != nil || patch != nil {
		t.Fatalf("(%v, %v, %v)", ev, patch, err)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %d, want 1", len(f.calls))
	}
	// The deadline sent is the bounded SearchTimeout (default 20s), never unbounded.
	if got := f.calls[0].Timeout; got != 20*time.Second {
		t.Errorf("timeout = %v", got)
	}
	d.Py.SearchTimeout = 3 * time.Second
	f.calls = nil
	_, _, _ = FetchJobChanges(context.Background(), d, pcWatch(), cs, false)
	if f.calls[0].Timeout != 3*time.Second {
		t.Errorf("configured timeout = %v", f.calls[0].Timeout)
	}

	// One contact resolved, then a timeout: the resolved one keeps its state and
	// the rest are left for the next poll (no state bump, so still first in LRU order).
	f2 := &pcFakePy{fn: func(n int, _ pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		if n == 0 {
			return pcSearch(), nil
		}
		return timeoutRes, nil
	}}
	ev, patch, err = FetchJobChanges(context.Background(), pcDeps(f2), pcWatch(), cs, false)
	if err != nil || len(ev) != 0 || len(f2.calls) != 2 {
		t.Fatalf("partial: (%v, %v, %v) calls=%d", ev, patch, err, len(f2.calls))
	}
	if st := pcMap(patch[JobChangeStateKey]); len(st) != 1 || st["k1"] == nil {
		t.Errorf("state = %v", st)
	}

	// Hiring: a timeout is an error, never a half result.
	if _, p, err := FetchHiringAndTech(context.Background(), pcDeps(f), pcWatch(), true, true, false); !errors.Is(err, ErrPyTimeout) || p != nil {
		t.Errorf("hiring: %v %v", p, err)
	}
	// Account group: a timed out partnership lookup fails only that collector.
	w := pcWatch()
	w.Config = map[string]any{"accounts": []any{map[string]any{"account_id": "a1", "company": "C"}, map[string]any{"account_id": "a2", "company": "D"}}}
	w.SignalTypes = []string{"partnership_hiring"}
	_, gp, err := FetchAccountGroup(context.Background(), pcDeps(f), w, false)
	if err != nil {
		t.Fatal(err)
	}
	pcEq(t, "failures", pcNorm(t, gp["_collector_failures"]), pcNorm(t, []any{"hiring_provider_error", "hiring_provider_error"}))
}

func TestPyworkerCancelledContextAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		cancel()
		return pyworker.ProviderResult{}, context.Canceled
	}}
	cs := []Contact{{Name: "A A", Key: "k1"}, {Name: "B B", Key: "k2"}}
	if _, _, err := FetchJobChanges(ctx, pcDeps(f), pcWatch(), cs, false); !errors.Is(err, context.Canceled) || errors.Is(err, ErrAllLookupsFailed) {
		t.Errorf("job change: %v", err)
	}
	w := pcWatch()
	w.Config = map[string]any{"accounts": []any{map[string]any{"account_id": "a1", "company": "C"}}}
	w.SignalTypes = []string{"partnership_hiring"}
	ctx2, cancel2 := context.WithCancel(context.Background())
	f2 := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		cancel2()
		return pyworker.ProviderResult{}, context.Canceled
	}}
	if _, _, err := FetchAccountGroup(ctx2, pcDeps(f2), w, false); !errors.Is(err, context.Canceled) {
		t.Errorf("account group: %v", err)
	}
}

func TestDDGSQueryNeverContainsSecrets(t *testing.T) {
	const token = "tok-SECRET-0123456789"
	const apiKey = "sk-live-SECRET-ABCDEF"
	var mu sync.Mutex
	var bodies []string
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "output": map[string]any{"fields": map[string]any{"results": []any{}}}, "cost_usd": 0})
	}))
	defer srv.Close()
	d := pcDeps(pyworker.New(srv.URL, token, srv.Client()))
	w := pcWatch()
	// Secret-looking values live next to the roster (config, cursor, company text only
	// in unrelated keys): none may reach the search.
	w.Config = map[string]any{"api_key": apiKey, "webhook_secret": apiKey, "contacts": []any{}}
	w.Cursor = map[string]any{"token": apiKey}
	cs := []Contact{
		{Name: "Ada Lovelace", Company: "Engines Inc", LinkedInURL: "https://linkedin.com/in/ada", Key: "li:ada"},
		{Name: "Grace Hopper", Company: "Navy", Key: "nm:grace hopper|navy"},
	}
	if _, _, err := FetchJobChanges(context.Background(), d, w, cs, false); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("calls = %d", len(bodies))
	}
	for _, b := range bodies {
		for _, secret := range []string{token, apiKey, "Bearer", "Authorization"} {
			if strings.Contains(b, secret) {
				t.Errorf("request body leaks %q: %s", secret, b)
			}
		}
		var req map[string]any
		_ = json.Unmarshal([]byte(b), &req)
		in := pcMap(req["input"])
		keys := []string{}
		for k := range in {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"max_results", "query"}) {
			t.Errorf("input keys = %v", keys)
		}
		if q, _ := in["query"].(string); !strings.HasSuffix(q, " site:linkedin.com/in") {
			t.Errorf("query = %q", q)
		}
	}
	for _, a := range auths {
		if a != "Bearer "+token {
			t.Errorf("Authorization header = %q (the token belongs in the header only)", a)
		}
	}
	// Errors never echo the token either.
	d2 := pcDeps(pyworker.New("http://127.0.0.1:1", token, nil))
	_, _, err := FetchJobChanges(context.Background(), d2, w, cs[:1], false)
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(fmt.Sprintf("%v %+v", d2.Py.Client, d2.Py), token) {
		t.Errorf("token leaked: %v", err)
	}
}

// ── account group ───────────────────────────────────────────────────────────

func TestAccountGroupFingerprintVectors(t *testing.T) {
	var v struct {
		Fingerprint []struct {
			HTMLPrefix string `json:"html_prefix"`
			HTMLLen    int    `json:"html_len"`
			HTML       string `json:"html"`
			HTMLRepeat *struct {
				Prefix string `json:"prefix"`
				Char   string `json:"char"`
				Count  int    `json:"count"`
			} `json:"html_repeat"`
			Fingerprint string  `json:"fingerprint"`
			_           float64 // keep keyed literals
		} `json:"fingerprint_html"`
		Terms []string `json:"partnership_terms"`
	}
	pcRead(t, "testdata/account_group_vectors.json", &v)
	if len(v.Fingerprint) != 16 {
		t.Fatalf("fingerprint vectors = %d", len(v.Fingerprint))
	}
	pcEq(t, "partnership terms", pcNorm(t, partnershipTerms), pcNorm(t, v.Terms))
	for i, c := range v.Fingerprint {
		html := c.HTML
		if c.HTMLRepeat != nil {
			html = c.HTMLRepeat.Prefix + strings.Repeat(c.HTMLRepeat.Char, c.HTMLRepeat.Count)
		}
		if got := FingerprintHTML(html); got != c.Fingerprint {
			t.Errorf("vector %d (%.40q): %s, want %s", i, html, got, c.Fingerprint)
		}
	}
	for _, c := range pcLoadExtra(t).FP {
		if got := FingerprintHTML(c.HTML); got != c.Fingerprint {
			t.Errorf("FingerprintHTML(%q) = %s, want %s", c.HTML, got, c.Fingerprint)
		}
	}
}

func TestPartnershipJobsVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Name   string `json:"name"`
			Result *struct {
				Success bool           `json:"success"`
				Error   *string        `json:"error"`
				Fields  map[string]any `json:"fields"`
			} `json:"provider_result"`
			Raises string         `json:"raises"`
			Want   map[string]any `json:"result"`
			Raised map[string]any `json:"result_raised"`
		} `json:"partnership_jobs"`
	}
	pcRead(t, "testdata/account_group_vectors.json", &v)
	if len(v.Cases) != 14 {
		t.Fatalf("vectors = %d", len(v.Cases))
	}
	for _, c := range v.Cases {
		f := &pcFakePy{fn: func(int, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
			switch {
			case c.Raises != "":
				return pyworker.ProviderResult{Status: "timeout", Error: "provider timed out"}, nil // an exception inside the provider run
			case c.Result == nil:
				return pyworker.ProviderResult{Status: "error", Error: "provider returned no result"}, nil
			case !c.Result.Success:
				msg := ""
				if c.Result.Error != nil {
					msg = *c.Result.Error
				}
				return pyworker.ProviderResult{Status: "error", Error: msg}, nil
			}
			return pcOK(c.Result.Fields), nil
		}}
		obs := PartnershipJobs(context.Background(), pcDeps(f), pcWatch(), 0, "Acme")
		got := map[string]any{"ok": obs.OK}
		if obs.OK {
			items := []any{}
			for _, it := range obs.Items {
				items = append(items, map[string]any{"id": it.ID, "title": it.Title, "url": it.URL})
			}
			got["items"] = items
		} else {
			got["error_class"] = obs.ErrorClass
		}
		want := c.Want
		if c.Raised != nil {
			// Python raises on a non-object hiring_signals (a crash); Go answers a
			// failed collector instead (plan 08 D4).
			want = map[string]any{"ok": false, "error_class": "hiring_payload_invalid"}
		}
		pcEq(t, c.Name, pcNorm(t, got), pcNorm(t, want))
		if f.calls[0].Provider != "jobspy_signals" || string(f.calls[0].Input) != `{"company":"Acme"}` {
			t.Errorf("%s: call = %s %s", c.Name, f.calls[0].Provider, f.calls[0].Input)
		}
	}
}

func TestAccountGroupObservationVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Name         string                               `json:"name"`
			Config       any                                  `json:"config"`
			SignalTypes  []string                             `json:"signal_types"`
			Cursor       any                                  `json:"cursor"`
			Backfill     bool                                 `json:"backfill"`
			Observations map[string]map[string]map[string]any `json:"observations"`
			Events       []any                                `json:"events"`
			Patch        map[string]any                       `json:"patch"`
		} `json:"fetch_account_group"`
	}
	pcRead(t, "testdata/account_group_vectors.json", &v)
	if len(v.Cases) != 26 {
		t.Fatalf("vectors = %d", len(v.Cases))
	}
	const now = 1788000000.5
	for _, c := range v.Cases {
		w := pcWatch()
		w.Config, _ = c.Config.(map[string]any)
		w.Cursor, _ = c.Cursor.(map[string]any)
		w.SignalTypes = c.SignalTypes
		observe := func(_ context.Context, _ int, account, _ map[string]any, requested map[string]bool) (map[string]agObservation, error) {
			out := map[string]agObservation{}
			for sig, m := range c.Observations[fmt.Sprint(account["account_id"])] {
				o := agObservation{OK: m["ok"] == true}
				o.ErrorClass, _ = m["error_class"].(string)
				o.Fingerprint, _ = m["fingerprint"].(string)
				o.URL, _ = m["url"].(string)
				o.ResolvedCIK, _ = m["resolved_cik"].(string)
				o.Cursor, _ = m["cursor"].(map[string]any)
				if items, ok := m["items"].([]any); ok {
					for _, it := range items {
						im := pcMap(it)
						id, _ := im["id"].(string)
						title, _ := im["title"].(string)
						url, _ := im["url"].(string)
						o.Items = append(o.Items, agItem{ID: id, Title: title, URL: url})
					}
				}
				if evs, ok := m["events"].([]any); ok {
					for _, e := range evs {
						em := pcMap(e)
						ev := DetectedEvent{}
						ev.NaturalEventID, _ = em["natural_event_id"].(string)
						ev.SignalType, _ = em["signal_type"].(string)
						ev.Title, _ = em["title"].(string)
						ev.Description, _ = em["description"].(string)
						ev.Source, _ = em["source"].(string)
						ev.SourceURL, _ = em["source_url"].(string)
						ev.Weight, _ = em["weight"].(float64)
						ev.OccurredAt, _ = em["occurred_at"].(float64)
						o.Events = append(o.Events, ev)
					}
				}
				out[sig] = o
			}
			return out, nil
		}
		ev, patch, err := fetchAccountGroupWith(context.Background(), w, c.Backfill, now, observe)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		// Python stamps pricing/partnership events with time.time(); the vectors carry
		// that as "now" and every other event keeps its own occurred_at.
		want := c.Events
		if want == nil {
			want = []any{}
		}
		pcEq(t, c.Name+" events", pcEvents(t, ev, now), pcNorm(t, want))
		pcEq(t, c.Name+" patch", pcNorm(t, patch), pcNorm(t, c.Patch))
	}
}

func TestAccountGroupPartialCollectorFailure(t *testing.T) {
	// funding: SEC down. partnership_hiring: pyworker down. pricing: fine.
	rt := pcRT(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
			Body: io.NopCloser(strings.NewReader("<h1>Pricing</h1><p>Pro $49</p>")), Request: r}, nil
	})
	d := pcDeps(nil)
	d.Client = &http.Client{Transport: rt}
	d.FormD = func(context.Context, string, string) ([]firmo.SecFiling, error) {
		return nil, errors.New("sec unavailable")
	}
	w := pcWatch()
	w.Config = map[string]any{"accounts": []any{
		map[string]any{"account_id": "acc-1", "lead_id": float64(14), "company": "Hot Pursuit Labs", "website": "https://hotpursuit.example.com/"},
	}}
	w.SignalTypes = []string{"partnership_hiring", "funding", "pricing_page_change"}
	w.Cursor = map[string]any{"collector_health": map[string]any{"acc-1:funding": map[string]any{"state": "failed", "attempt_count": float64(2)}}}

	ev, patch, err := FetchAccountGroup(context.Background(), d, w, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 0 {
		t.Errorf("events = %v", ev)
	}
	pcEq(t, "failures", pcNorm(t, patch["_collector_failures"]), pcNorm(t, []any{"sec_provider_unavailable", "hiring_provider_unavailable"}))
	pcEq(t, "health", pcNorm(t, patch["collector_health"]), pcNorm(t, map[string]any{
		"acc-1:funding":             map[string]any{"state": "failed", "attempt_count": 3, "last_error_class": "sec_provider_unavailable"},
		"acc-1:partnership_hiring":  map[string]any{"state": "failed", "attempt_count": 1, "last_error_class": "hiring_provider_unavailable"},
		"acc-1:pricing_page_change": map[string]any{"state": "healthy", "attempt_count": 1, "last_error_class": nil},
	}))
	acct := pcMap(pcMap(patch["account_group"])["acc-1"])
	if acct["bootstrapped"] != true || acct["pricing_fingerprint"] != FingerprintHTML("<h1>Pricing</h1><p>Pro $49</p>") ||
		acct["pricing_url"] != "https://hotpursuit.example.com/pricing" {
		t.Errorf("healthy collector did not record its state: %v", acct)
	}
	if _, has := acct["sec"]; has {
		t.Errorf("a failed collector must not write its cursor slice: %v", acct)
	}
}

// ── pricing fetch ───────────────────────────────────────────────────────────

type pcRT func(*http.Request) (*http.Response, error)

func (f pcRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type pcTimeoutErr struct{}

func (pcTimeoutErr) Error() string   { return "timed out" }
func (pcTimeoutErr) Timeout() bool   { return true }
func (pcTimeoutErr) Temporary() bool { return true }

type pcResolver map[string][]netip.Addr

func (r pcResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestPricingFetchBlockedHost(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()

	d := pcDeps(nil)
	d.AppEnv = "production"
	d.Client = egress.NewClientForEnv("production", nil)
	for _, u := range []string{
		srv.URL + "/pricing", // 127.0.0.1 literal
		"http://localhost/pricing", "http://127.1/pricing", "http://2130706433/pricing", "http://[::1]/pricing",
		"http://[::ffff:10.0.0.1]/pricing", "http://169.254.169.254/pricing", "http://metadata.google.internal/pricing",
		"http://10.0.0.7/pricing", "ftp://example.com/pricing", "file:///etc/passwd", "/pricing", "",
	} {
		o := FetchPricing(context.Background(), d, u)
		if o.OK || o.ErrorClass != "pricing_url_blocked" {
			t.Errorf("%q: %+v", u, o)
		}
	}
	if hits != 0 {
		t.Errorf("the loopback server was reached %d times", hits)
	}

	// DNS rebinding: a public-looking name that resolves private is refused at dial
	// time, before any connection.
	dialed := 0
	res := pcResolver{"acme.example.com": {netip.MustParseAddr("10.0.0.7")}, "mixed.example.com": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}}
	d.Client = egress.NewClientWith("production", nil, res, func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, errors.New("dial reached")
	})
	for _, u := range []string{"https://acme.example.com/pricing", "https://mixed.example.com/pricing", "http://acme.example.com/pricing"} {
		o := FetchPricing(context.Background(), d, u)
		if o.OK || o.ErrorClass != "pricing_url_blocked" {
			t.Errorf("%q: %+v", u, o)
		}
	}
	if dialed != 0 {
		t.Errorf("a blocked address was dialled %d times", dialed)
	}
	// A public name whose dial fails is a transport error, not a block.
	res["ok.example.com"] = []netip.Addr{netip.MustParseAddr("93.184.216.34")}
	if o := FetchPricing(context.Background(), d, "https://ok.example.com/pricing"); o.ErrorClass != "pricing_transport_error" || dialed != 1 {
		t.Errorf("transport error: %+v dialed=%d", o, dialed)
	}
	// The test allow-list only works in the test environment.
	d.AppEnv = "production"
	d.EgressAllowHosts = []string{"127.0.0.1"}
	d.Client = egress.NewClientForEnv("production", d.EgressAllowHosts)
	if o := FetchPricing(context.Background(), d, srv.URL+"/pricing"); o.ErrorClass != "pricing_url_blocked" || hits != 0 {
		t.Errorf("allow-list honoured in production: %+v", o)
	}
	d.AppEnv = "test"
	d.Client = egress.NewClientForEnv("test", d.EgressAllowHosts)
	if o := FetchPricing(context.Background(), d, srv.URL+"/pricing"); hits != 1 || o.ErrorClass == "pricing_url_blocked" {
		t.Errorf("allow-listed test host: %+v hits=%d", o, hits)
	}
}

type pcExchange struct {
	Request  map[string]any `json:"request"`
	Response map[string]any `json:"response"`
	DNS      map[string]any `json:"dns"`
}

func TestPricingTranscripts(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/transcripts/pricing/*.json")
	if len(files) < 23 {
		t.Fatalf("pricing transcripts = %d", len(files))
	}
	// Python follows redirects hop by hop; Go never follows one (plan 08 D4), so a
	// 3xx is that status as the error class.
	redirects := map[string]bool{"redirect_followed_manually": true, "redirect_relative_location": true, "redirect_loop_too_many": true, "redirect_to_private_blocked": true}
	for _, f := range files {
		var tr struct {
			Scenario  string         `json:"scenario"`
			Input     map[string]any `json:"input"`
			Exchanges []pcExchange   `json:"http_exchanges"`
			Result    map[string]any `json:"result"`
		}
		pcRead(t, f, &tr)
		t.Run(tr.Scenario, func(t *testing.T) {
			url := tr.Input["url"].(string)
			var seq []pcExchange
			dns := pcResolver{}
			for _, e := range tr.Exchanges {
				if e.DNS != nil {
					var addrs []netip.Addr
					for _, a := range e.DNS["answers"].([]any) {
						addrs = append(addrs, netip.MustParseAddr(a.(string)))
					}
					dns[e.DNS["host"].(string)] = addrs
				} else {
					seq = append(seq, e)
				}
			}
			d := pcDeps(nil)
			d.AppEnv = "production"
			next := 0
			d.Client = &http.Client{Transport: pcRT(func(r *http.Request) (*http.Response, error) {
				if next >= len(seq) {
					t.Fatalf("unscripted request %s", r.URL)
				}
				e := seq[next]
				next++
				if r.URL.String() != e.Request["url"] || r.Header.Get("User-Agent") != "OpenGTM Signal Monitor/1.0" || r.Method != "GET" {
					t.Errorf("request = %s %s UA=%q, recorded %v", r.Method, r.URL, r.Header.Get("User-Agent"), e.Request)
				}
				if raises, _ := e.Response["raises"].(string); raises != "" {
					if raises == "ReadTimeout" {
						return nil, &net.OpError{Op: "read", Err: pcTimeoutErr{}}
					}
					return nil, errors.New("connection refused")
				}
				h := http.Header{}
				for k, v := range pcMap(e.Response["headers"]) {
					h.Set(k, v.(string))
				}
				body, _ := e.Response["body"].(string)
				return &http.Response{StatusCode: int(e.Response["status"].(float64)), Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			if tr.Scenario == "blocked_private_dns" {
				d.Client = egress.NewClientWith("production", nil, dns, func(context.Context, string, string) (net.Conn, error) {
					t.Error("dialled a private address")
					return nil, errors.New("dial")
				})
			}
			o := FetchPricing(context.Background(), d, url)
			got := map[string]any{"ok": o.OK}
			if o.OK {
				got["present"] = o.Fingerprint != "missing"
				got["url"] = o.URL
				got["fingerprint"] = o.Fingerprint
			} else {
				got["error_class"] = o.ErrorClass
			}
			want := tr.Result
			if redirects[tr.Scenario] {
				want = map[string]any{"ok": false, "error_class": fmt.Sprintf("pricing_http_%d", int(seq[0].Response["status"].(float64)))}
			}
			pcEq(t, tr.Scenario, pcNorm(t, got), pcNorm(t, want))
		})
	}
}

func TestPricingBodyBoundedAndCharset(t *testing.T) {
	big := strings.Repeat("a ", 700_000) // 1.4 MB
	d := pcDeps(nil)
	served := 0
	d.Client = &http.Client{Transport: pcRT(func(r *http.Request) (*http.Response, error) {
		served++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(big)), Request: r}, nil
	})}
	o := FetchPricing(context.Background(), d, "https://acme.example.com/pricing")
	if !o.OK || o.Fingerprint != FingerprintHTML(big[:1<<20]) {
		t.Errorf("oversize body: %+v", o)
	}
	// ISO-8859-1 declared in the header decodes like httpx.
	d.Client = &http.Client{Transport: pcRT(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=iso-8859-1"}},
			Body: io.NopCloser(strings.NewReader("Caf\xe9 \x80")), Request: r}, nil
	})}
	if o := FetchPricing(context.Background(), d, "https://acme.example.com/pricing"); o.Fingerprint != FingerprintHTML("Café \u0080") {
		t.Errorf("latin-1: %+v", o)
	}
	// A parent cancel is not a pricing failure class.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := FetchAccountGroup(ctx, pcDeps(nil), PyWatch{WatchView: WatchView{ID: "w"}, Config: map[string]any{"accounts": []any{map[string]any{"account_id": "a"}}}, SignalTypes: []string{"pricing_page_change"}}, false); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
