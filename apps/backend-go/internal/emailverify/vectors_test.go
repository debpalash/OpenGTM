package emailverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/enrichcache"
)

// ---- vector loading -------------------------------------------------------

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// jsonEqual compares got (any Go value) with the Python-recorded JSON, ignoring
// formatting and int/float spelling.
func jsonEqual(t *testing.T, label string, got any, want json.RawMessage) {
	t.Helper()
	gb, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	var g, w any
	if err := json.Unmarshal(gb, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s:\n got  %s\n want %s", label, gb, compact(want))
	}
}

func compact(raw json.RawMessage) string {
	var v any
	_ = json.Unmarshal(raw, &v)
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- scripted DNS + SMTP world (mirrors the Python generator's fakes) -------

type smtpSpec struct {
	RandomPrefix string         `json:"random_prefix"`
	RandomCode   *int           `json:"random_code"`
	RcptDefault  *int           `json:"rcpt_default"`
	Rcpt         map[string]int `json:"rcpt"`
	Mail         *int           `json:"mail"`
	ConnectError string         `json:"connect_error"`
	DisconnectOn []string       `json:"disconnect_on"`
	ExcOn        []string       `json:"exc_on"`
	QuitError    bool           `json:"quit_error"`
}

type world struct {
	mx   map[string]json.RawMessage // list of [pref, host] or an error string
	smtp map[string]smtpSpec
	log  [][]any
}

func newWorld(mx map[string]json.RawMessage, smtp map[string]smtpSpec) *world {
	return &world{mx: mx, smtp: smtp}
}

func (w *world) LookupMX(_ context.Context, domain string) ([]*net.MX, error) {
	w.log = append(w.log, []any{"dns", domain, "MX"})
	raw, ok := w.mx[domain]
	if !ok {
		return nil, errors.New("NXDOMAIN")
	}
	var msg string
	if json.Unmarshal(raw, &msg) == nil {
		return nil, errors.New(msg)
	}
	var recs [][2]any
	if err := json.Unmarshal(raw, &recs); err != nil {
		return nil, err
	}
	out := []*net.MX{}
	for _, r := range recs {
		out = append(out, &net.MX{Pref: uint16(r[0].(float64)), Host: r[1].(string)})
	}
	return out, nil
}

type fakeSession struct {
	w    *world
	host string
}

func (w *world) newSession() SMTPSession {
	w.log = append(w.log, []any{"smtp.new"})
	return &fakeSession{w: w}
}

func (s *fakeSession) Connect(_ context.Context, host string, port int) error {
	s.w.log = append(s.w.log, []any{"connect", host, port})
	if s.w.smtp[host].ConnectError != "" {
		return errors.New("connect failed: " + s.w.smtp[host].ConnectError)
	}
	s.host = host
	return nil
}
func (s *fakeSession) EhloOrHelo(string) error {
	s.w.log = append(s.w.log, []any{"ehlo_or_helo"})
	return nil
}
func (s *fakeSession) Helo(name string) error {
	s.w.log = append(s.w.log, []any{"helo", name})
	return nil
}
func (s *fakeSession) Mail(from string) (int, error) {
	s.w.log = append(s.w.log, []any{"mail", from})
	if c := s.w.smtp[s.host].Mail; c != nil {
		return *c, nil
	}
	return 250, nil
}
func (s *fakeSession) Rcpt(addr string) (int, error) {
	s.w.log = append(s.w.log, []any{"rcpt", addr})
	spec := s.w.smtp[s.host]
	for _, a := range spec.DisconnectOn {
		if a == addr {
			return 0, ErrDisconnected
		}
	}
	for _, a := range spec.ExcOn {
		if a == addr {
			return 0, ErrSMTP
		}
	}
	if spec.RandomPrefix != "" && strings.HasPrefix(addr, spec.RandomPrefix) {
		if spec.RandomCode != nil {
			return *spec.RandomCode, nil
		}
		return 550, nil
	}
	if c, ok := spec.Rcpt[addr]; ok {
		return c, nil
	}
	if spec.RcptDefault != nil {
		return *spec.RcptDefault, nil
	}
	return 250, nil
}
func (s *fakeSession) Quit() error {
	s.w.log = append(s.w.log, []any{"quit"})
	if s.w.smtp[s.host].QuitError {
		return ErrDisconnected
	}
	return nil
}
func (s *fakeSession) Close() { s.w.log = append(s.w.log, []any{"close"}) }

func (w *world) deps(smtpDisabled bool) Deps {
	d := NewDeps()
	d.Resolver = w
	d.NewSession = w.newSession
	d.SMTPDisabled = smtpDisabled
	d.RandLocal = func() string { return "abcdefghijklmnop" }
	return d
}

// ---- classify --------------------------------------------------------------

type classRow struct {
	Input               string `json:"input"`
	Email               string `json:"email"`
	ValidSyntax         bool   `json:"valid_syntax"`
	LocalPart           string `json:"local_part"`
	Domain              string `json:"domain"`
	IsRole              bool   `json:"is_role"`
	IsFree              bool   `json:"is_free"`
	IsDisposable        bool   `json:"is_disposable"`
	IsPersonalCorporate bool   `json:"is_personal_corporate"`
}

type fileStat struct {
	Count  int      `json:"count"`
	Sha    string   `json:"sha256_of_sorted_lines"`
	Head   []string `json:"head"`
}

func TestClassifyVectors(t *testing.T) {
	var v struct {
		Classify  []classRow          `json:"classify"`
		DataFiles map[string]fileStat `json:"data_files"`
		ResolveMX []struct {
			Name  string                     `json:"name"`
			MX    map[string]json.RawMessage `json:"mx"`
			Res   *string                    `json:"result"`
			HasMX bool                       `json:"has_mx"`
			Blank *string                    `json:"blank"`
		} `json:"resolve_mx"`
	}
	loadJSON(t, "classify_vectors.json", &v)
	if len(v.Classify) < 20 {
		t.Fatalf("vectors look empty: %d", len(v.Classify))
	}
	for _, r := range v.Classify {
		c := Classify(r.Input)
		got := classRow{Input: r.Input, Email: c.Email, ValidSyntax: c.ValidSyntax, LocalPart: c.LocalPart, Domain: c.Domain,
			IsRole: c.IsRole, IsFree: c.IsFree, IsDisposable: c.IsDisposable, IsPersonalCorporate: c.IsPersonalCorporate()}
		if got != r {
			t.Errorf("Classify(%q) = %+v, want %+v", r.Input, got, r)
		}
	}
	sets := map[string]map[string]struct{}{"role_accounts": roleAccounts(), "free_providers": freeProviders(), "disposable_domains": disposableDomains()}
	for name, want := range v.DataFiles {
		s := sets[name]
		lines := make([]string, 0, len(s))
		for k := range s {
			lines = append(lines, k)
		}
		sort.Strings(lines)
		sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		if len(lines) != want.Count || hex.EncodeToString(sum[:]) != want.Sha {
			t.Errorf("%s: count %d sha %s, want %d %s", name, len(lines), hex.EncodeToString(sum[:]), want.Count, want.Sha)
		}
		if !reflect.DeepEqual(lines[:len(want.Head)], want.Head) {
			t.Errorf("%s head differs", name)
		}
	}
	for _, c := range v.ResolveMX {
		w := newWorld(c.MX, nil)
		d := w.deps(false)
		ctx := context.Background()
		got := ResolveMX(ctx, d, "a.com")
		want := ""
		if c.Res != nil {
			want = *c.Res
		}
		if got != want {
			t.Errorf("%s: ResolveMX = %q, want %q", c.Name, got, want)
		}
		if HasMX(ctx, d, "a.com") != c.HasMX {
			t.Errorf("%s: HasMX = %v", c.Name, !c.HasMX)
		}
		if ResolveMX(ctx, d, "") != "" {
			t.Errorf("%s: blank domain must resolve to none", c.Name)
		}
	}
}

// ---- SMTP probe, verify_email, pick_best_email, SmtpVerifier ---------------

type probeJSON struct {
	Domain    string           `json:"domain"`
	MXHost    *string          `json:"mx_host"`
	Reachable bool             `json:"reachable"`
	CatchAll  bool             `json:"catch_all"`
	Results   map[string]*bool `json:"results"`
}

func (p DomainProbe) wire() probeJSON {
	var mx *string
	if p.MXHost != "" {
		mx = &p.MXHost
	}
	return probeJSON{Domain: p.Domain, MXHost: mx, Reachable: p.Reachable, CatchAll: p.CatchAll, Results: p.Results}
}

func TestSMTPProbeVectors(t *testing.T) {
	var v struct {
		Constants struct {
			Helo, MailFrom, RandomLocal string
			Port                        int
		} `json:"constants"`
		Probe []struct {
			Name        string                     `json:"name"`
			MX          map[string]json.RawMessage `json:"mx"`
			SMTP        map[string]smtpSpec        `json:"smtp"`
			SMTPEnabled bool                       `json:"smtp_enabled"`
			Domain      string                     `json:"domain"`
			Candidates  []string                   `json:"candidates"`
			ProbeOut    json.RawMessage            `json:"probe"`
			Log         json.RawMessage            `json:"log"`
		} `json:"probe_domain"`
	}
	raw, err := os.ReadFile("testdata/smtp_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	// the constants block uses snake_case keys
	var consts struct {
		Constants struct {
			Helo   string `json:"helo"`
			Mail   string `json:"mail_from"`
			Random string `json:"random_local"`
			Port   int    `json:"port"`
		} `json:"constants"`
	}
	_ = json.Unmarshal(raw, &consts)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if consts.Constants.Helo != "verify.yupcha.com" || consts.Constants.Mail != "verify@yupcha.com" || consts.Constants.Port != 25 {
		t.Fatalf("constants: %+v", consts.Constants)
	}
	if len(v.Probe) != 25 {
		t.Fatalf("expected 25 probe vectors, got %d", len(v.Probe))
	}
	for _, c := range v.Probe {
		t.Run(c.Name, func(t *testing.T) {
			w := newWorld(c.MX, c.SMTP)
			d := w.deps(!c.SMTPEnabled)
			ctx := context.Background()
			cands := c.Candidates
			if c.Name == "second_probe_uses_catchall_cache" {
				if _, err := ProbeDomain(ctx, d, "acme.com", []string{"a@acme.com"}); err != nil {
					t.Fatal(err)
				}
				w.log = nil
				cands = []string{"b@acme.com"}
			}
			p, err := ProbeDomain(ctx, d, c.Domain, cands)
			if err != nil {
				t.Fatal(err)
			}
			jsonEqual(t, "probe", p.wire(), c.ProbeOut)
			logs := w.log
			if logs == nil {
				logs = [][]any{}
			}
			jsonEqual(t, "log", logs, c.Log)
		})
	}
}

type emailCheckJSON struct {
	Email          string          `json:"email"`
	Confidence     string          `json:"confidence"`
	Deliverable    *bool           `json:"deliverable"`
	CatchAll       bool            `json:"catch_all"`
	Reachable      bool            `json:"reachable"`
	Classification *classification `json:"classification"`
}

type classification struct {
	Email       string `json:"email"`
	ValidSyntax bool   `json:"valid_syntax"`
	LocalPart   string `json:"local_part"`
	Domain      string `json:"domain"`
	IsRole      bool   `json:"is_role"`
	IsFree      bool   `json:"is_free"`
	IsDisposable bool  `json:"is_disposable"`
}

func (r EmailCheck) wire() emailCheckJSON {
	out := emailCheckJSON{Email: r.Email, Confidence: r.Confidence, Deliverable: r.Deliverable, CatchAll: r.CatchAll, Reachable: r.Reachable}
	if r.Classification != nil {
		c := r.Classification
		out.Classification = &classification{c.Email, c.ValidSyntax, c.LocalPart, c.Domain, c.IsRole, c.IsFree, c.IsDisposable}
	}
	return out
}

func scenarioWorld(scenario string) *world {
	R := "abcdefghijklmnop"
	n250, n550, n450 := 250, 550, 450
	_ = n550
	mx := map[string]json.RawMessage{"acme.com": json.RawMessage(`[[10,"m"]]`), "catchall.com": json.RawMessage(`[[10,"c"]]`)}
	if scenario == "good" {
		rc := 550
		return newWorld(mx, map[string]smtpSpec{
			"m": {RandomPrefix: R, RandomCode: &rc, Rcpt: map[string]int{"a@acme.com": 250, "bad@acme.com": 550, "z@acme.com": n450}},
			"c": {RcptDefault: &n250},
		})
	}
	return newWorld(mx, map[string]smtpSpec{"m": {ConnectError: "oserror"}, "c": {ConnectError: "oserror"}})
}

func TestVerifyEmailAndSMTPVerifierVectors(t *testing.T) {
	var v struct {
		Verify []struct {
			Email    string          `json:"email"`
			Scenario string          `json:"scenario"`
			Result   json.RawMessage `json:"result"`
		} `json:"verify_email"`
		SV []struct {
			Email    string          `json:"email"`
			Scenario string          `json:"scenario"`
			Result   json.RawMessage `json:"result"`
		} `json:"smtp_verifier"`
	}
	loadJSON(t, "smtp_vectors.json", &v)
	if len(v.Verify) != 16 || len(v.SV) != 16 {
		t.Fatalf("vectors: %d %d", len(v.Verify), len(v.SV))
	}
	ctx := context.Background()
	for _, c := range v.Verify {
		w := scenarioWorld(c.Scenario)
		r, err := VerifyEmail(ctx, w.deps(false), c.Email)
		if err != nil {
			t.Fatal(err)
		}
		jsonEqual(t, fmt.Sprintf("VerifyEmail(%s,%s)", c.Email, c.Scenario), r.wire(), c.Result)
	}
	for _, c := range v.SV {
		w := scenarioWorld(c.Scenario)
		r, err := SMTPVerifier(w.deps(false)).Verify(ctx, c.Email)
		if err != nil {
			t.Fatal(err)
		}
		jsonEqual(t, fmt.Sprintf("SMTPVerifier(%s,%s)", c.Email, c.Scenario), r, c.Result)
	}
}

func TestPickBestEmailVectors(t *testing.T) {
	var v struct {
		Pick []struct {
			Name        string                     `json:"name"`
			Candidates  []string                   `json:"candidates"`
			MX          map[string]json.RawMessage `json:"mx"`
			SMTP        map[string]smtpSpec        `json:"smtp"`
			SMTPEnabled bool                       `json:"smtp_enabled"`
			Result      json.RawMessage            `json:"result"`
		} `json:"pick_best_email"`
	}
	loadJSON(t, "smtp_vectors.json", &v)
	if len(v.Pick) != 10 {
		t.Fatalf("vectors: %d", len(v.Pick))
	}
	for _, c := range v.Pick {
		w := newWorld(c.MX, c.SMTP)
		r, err := PickBestEmail(context.Background(), w.deps(!c.SMTPEnabled), c.Candidates)
		if err != nil {
			t.Fatal(err)
		}
		jsonEqual(t, c.Name, r.wire(), c.Result)
	}
}

// ---- cascade ---------------------------------------------------------------

type kindErr struct{ kind string }

func (e kindErr) Error() string { return "scripted failure: " + e.kind }
func (e kindErr) Kind() string  { return e.kind }

type scripted struct {
	name   string
	status string
	source string
	conf   float64
	detail string
	raises string
	avail  any // bool or "raises"
	calls  int
}

func (s *scripted) Name() string { return s.name }
func (s *scripted) Available(context.Context) (bool, error) {
	if s.avail == "raises" {
		return false, kindErr{"RuntimeError"}
	}
	if b, ok := s.avail.(bool); ok {
		return b, nil
	}
	return true, nil
}
func (s *scripted) Verify(_ context.Context, email string) (Result, error) {
	s.calls++
	if s.raises != "" {
		return Result{}, kindErr{s.raises}
	}
	return Result{Email: email, Status: s.status, Source: s.source, Confidence: s.conf, Detail: s.detail}, nil
}

type specRow struct {
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
	Available json.RawMessage `json:"available"`
}

func (r specRow) build(t *testing.T) *scripted {
	t.Helper()
	s := &scripted{name: r.Name, avail: true}
	var raises struct {
		Raises string `json:"raises"`
	}
	if json.Unmarshal(r.Spec, &raises) == nil && raises.Raises != "" {
		s.raises = raises.Raises
	} else {
		var arr []any
		if err := json.Unmarshal(r.Spec, &arr); err != nil || len(arr) != 4 {
			t.Fatalf("bad spec %s", r.Spec)
		}
		s.status, s.source, s.detail = arr[0].(string), arr[1].(string), arr[3].(string)
		s.conf, _ = arr[2].(float64)
	}
	var b bool
	if json.Unmarshal(r.Available, &b) == nil {
		s.avail = b
	} else {
		s.avail = "raises"
	}
	return s
}

func resultWire(r Result) map[string]any {
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["is_definitive"] = r.IsDefinitive()
	m["deliverable"] = r.Deliverable()
	return m
}

func TestCascadeVectors(t *testing.T) {
	var v struct {
		Cascade []struct {
			Name      string          `json:"name"`
			Email     *string         `json:"email"`
			Verifiers []specRow       `json:"verifiers"`
			Calls     []int           `json:"calls"`
			Result    json.RawMessage `json:"result"`
		} `json:"cascade"`
		Vendor []struct {
			Raw    *string `json:"raw"`
			Status string  `json:"status"`
		} `json:"vendor_status"`
	}
	loadJSON(t, "cascade_vectors.json", &v)
	if len(v.Cascade) != 14 || len(v.Vendor) != 22 {
		t.Fatalf("vectors: %d %d", len(v.Cascade), len(v.Vendor))
	}
	for _, c := range v.Cascade {
		t.Run(c.Name, func(t *testing.T) {
			var chain []Verifier
			var all []*scripted
			for _, r := range c.Verifiers {
				s := r.build(t)
				all = append(all, s)
				chain = append(chain, s)
			}
			email := ""
			if c.Email != nil {
				email = *c.Email
			}
			res, err := CascadeVerifiers(context.Background(), NewDeps(), "ws1", email, chain)
			if err != nil {
				t.Fatal(err)
			}
			jsonEqual(t, "result", resultWire(res), c.Result)
			for i, s := range all {
				if s.calls != c.Calls[i] {
					t.Errorf("verifier %s called %d times, want %d", s.name, s.calls, c.Calls[i])
				}
			}
		})
	}
	for _, c := range v.Vendor {
		raw := ""
		if c.Raw != nil {
			raw = *c.Raw
		}
		if got := StatusFromVendor(raw); got != c.Status {
			t.Errorf("StatusFromVendor(%q) = %q, want %q", raw, got, c.Status)
		}
	}
}

func TestAvailableVerifiersVectors(t *testing.T) {
	var v struct {
		Avail []struct {
			Name          string   `json:"name"`
			Available     []string `json:"available"`
			AfterRegister []string `json:"after_register"`
		} `json:"available_verifiers"`
	}
	loadJSON(t, "cascade_vectors.json", &v)
	// the Python generator pairs each case with (name, available) specs
	specs := map[string][]struct {
		n string
		a any
	}{
		"filters_unavailable":          {{"a", true}, {"b", false}, {"c", true}},
		"raising_is_available_skipped": {{"a", "raises"}, {"b", true}},
		"none":                         {},
	}
	for _, c := range v.Avail {
		reg := NewRegistry()
		for _, s := range specs[c.Name] {
			reg.Register(&scripted{name: s.n, avail: s.a, status: "valid"}, false)
		}
		names := func(vs []Verifier) []string {
			out := []string{}
			for _, x := range vs {
				out = append(out, x.Name())
			}
			return out
		}
		if got := names(reg.Available(context.Background())); !reflect.DeepEqual(got, c.Available) {
			t.Errorf("%s: available %v, want %v", c.Name, got, c.Available)
		}
		reg.Register(&scripted{name: "front", status: "valid", avail: true}, true)
		reg.Register(&scripted{name: "back", status: "valid", avail: true}, false)
		if got := names(reg.All()); !reflect.DeepEqual(got, c.AfterRegister) {
			t.Errorf("%s: after register %v, want %v", c.Name, got, c.AfterRegister)
		}
	}
}

// memCache is the in-memory VerifyCache used for the clocked cache vectors; it
// follows the same key (enrichcache.EmailKey), TTL (EmailTTL) and freshness
// (expires_at >= now) rules as the Postgres cache.
type memCache struct {
	now  *time.Time
	rows map[string]memRow
	puts int
}
type memRow struct {
	e   CacheEntry
	exp time.Time
}

func (m *memCache) key(ws, email, verifier string) string {
	return ws + "|" + enrichcache.EmailKey(email, verifier)
}
func (m *memCache) Get(_ context.Context, ws, email, verifier string) (CacheEntry, bool, error) {
	if email == "" || verifier == "" {
		return CacheEntry{}, false, nil
	}
	r, ok := m.rows[m.key(ws, email, verifier)]
	if !ok || r.exp.Before(*m.now) {
		return CacheEntry{}, false, nil
	}
	return r.e, true, nil
}
func (m *memCache) Put(_ context.Context, ws, email, verifier string, e CacheEntry) error {
	if email == "" || verifier == "" || e.Status == "" {
		return nil
	}
	m.puts++
	m.rows[m.key(ws, email, verifier)] = memRow{e: e, exp: m.now.Add(enrichcache.EmailTTL(e.Status))}
	return nil
}

func TestCascadeCachedVectors(t *testing.T) {
	var v struct {
		Base   float64 `json:"clock_base"`
		Cached []struct {
			Name  string `json:"name"`
			Steps []struct {
				Email     string          `json:"email"`
				At        float64         `json:"at"`
				Verifiers []specRow       `json:"verifiers"`
				Calls     []int           `json:"calls"`
				Result    json.RawMessage `json:"result"`
			} `json:"steps"`
		} `json:"cached"`
	}
	loadJSON(t, "cascade_vectors.json", &v)
	if len(v.Cached) != 10 {
		t.Fatalf("vectors: %d", len(v.Cached))
	}
	for _, c := range v.Cached {
		t.Run(c.Name, func(t *testing.T) {
			var now time.Time
			cache := &memCache{now: &now, rows: map[string]memRow{}}
			for i, st := range c.Steps {
				now = time.Unix(int64(v.Base+st.At), 0).UTC()
				d := NewDeps()
				d.Cache = cache
				d.Registry = NewRegistry()
				var all []*scripted
				for _, r := range st.Verifiers {
					s := r.build(t)
					all = append(all, s)
					if s.name == "reacher" {
						rv := s
						d.Reacher = func(context.Context, string) Verifier { return rv }
					} else {
						d.Registry.Register(s, false)
					}
				}
				res, err := Cascade(context.Background(), d, "ws1", st.Email)
				if err != nil {
					t.Fatal(err)
				}
				jsonEqual(t, fmt.Sprintf("step %d result", i), resultWire(res), st.Result)
				for j, s := range all {
					if s.calls != st.Calls[j] {
						t.Errorf("step %d: %s called %d times, want %d", i, s.name, s.calls, st.Calls[j])
					}
				}
			}
		})
	}
}

func TestCascadeHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	v1 := &scripted{name: "v1", status: "unknown", source: "v1", detail: "soft", avail: true}
	cancelling := &cancelVerifier{cancel: cancel}
	v3 := &scripted{name: "v3", status: "valid", source: "v3", avail: true}
	d := NewDeps()
	_, err := CascadeVerifiers(ctx, d, "ws1", "a@b.co", []Verifier{v1, cancelling, v3})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if v3.calls != 0 {
		t.Fatal("verifier after cancellation must not run")
	}
	// already-cancelled context: nothing runs at all
	v1.calls = 0
	if _, err := CascadeVerifiers(ctx, d, "ws1", "a@b.co", []Verifier{v1}); !errors.Is(err, context.Canceled) || v1.calls != 0 {
		t.Fatalf("pre-cancelled: err=%v calls=%d", err, v1.calls)
	}
}

type cancelVerifier struct{ cancel context.CancelFunc }

func (c *cancelVerifier) Name() string                              { return "cancel" }
func (c *cancelVerifier) Available(context.Context) (bool, error) { return true, nil }
func (c *cancelVerifier) Verify(ctx context.Context, _ string) (Result, error) {
	c.cancel()
	return Result{}, ctx.Err()
}

// ---- deliverability --------------------------------------------------------

type verdictSpec struct {
	status, source string
	conf           float64
	detail         string
	raises         bool
}

func (v verdictSpec) deps(t *testing.T) Deps {
	d := NewDeps()
	d.Registry = NewRegistry()
	if v.raises {
		d.cascade = func(context.Context, Deps, string, string) (Result, error) { return Result{}, errors.New("boom") }
		return d
	}
	d.Registry.Register(&scripted{name: v.source, status: v.status, source: v.source, conf: v.conf, detail: v.detail, avail: true}, false)
	return d
}

func TestDeliverabilityVectors(t *testing.T) {
	var v struct {
		Verify []struct {
			Email   string          `json:"email"`
			Verdict string          `json:"verdict"`
			Result  json.RawMessage `json:"result"`
		} `json:"verify_deliverability"`
		Tag []struct {
			Name    string          `json:"name"`
			Verdict string          `json:"verdict"`
			Email   string          `json:"email"`
			Result  json.RawMessage `json:"result"`
			Lead    json.RawMessage `json:"lead"`
		} `json:"tag_email_confidence"`
	}
	loadJSON(t, "deliverability_vectors.json", &v)
	if len(v.Verify) != 48 || len(v.Tag) != 15 {
		t.Fatalf("vectors: %d %d", len(v.Verify), len(v.Tag))
	}
	verdicts := map[string]verdictSpec{
		"valid":     {"valid", "smtp", 0.95, "rcpt_accepted", false},
		"invalid":   {"invalid", "smtp", 0.9, "rcpt_rejected", false},
		"catch_all": {"catch_all", "smtp", 0.5, "catch_all", false},
		"unknown":   {"unknown", "", 0.0, "no_verifier", false},
		"raises":    {raises: true},
		"weird":     {"weird", "x", 0, "", false},
	}
	for i, c := range v.Verify {
		got, err := Deliverability(context.Background(), verdicts[c.Verdict].deps(t), "ws1", c.Email)
		if err != nil {
			t.Fatal(err)
		}
		jsonEqual(t, fmt.Sprintf("#%d %q/%s", i, c.Email, c.Verdict), got.wire(), c.Result)
	}
	tagVerdicts := map[string]verdictSpec{
		"valid":   {"valid", "smtp", 0.95, "ok", false},
		"invalid": {"invalid", "smtp", 0.9, "rej", false},
		"unknown": {"unknown", "", 0.0, "x", false},
	}
	for _, c := range v.Tag {
		lead := map[string]any{"company": "X", "email": c.Email, "email_confidence": "old", "email_provider": "hunter"}
		got, err := TagEmailConfidence(context.Background(), tagVerdicts[c.Verdict].deps(t), "ws1", lead)
		if err != nil {
			t.Fatal(err)
		}
		if string(c.Result) == "null" {
			if got != nil {
				t.Errorf("%s/%s: expected nil result", c.Name, c.Verdict)
			}
		} else {
			if got == nil {
				t.Fatalf("%s/%s: nil result", c.Name, c.Verdict)
			}
			jsonEqual(t, c.Name+"/"+c.Verdict+" result", got.wire(), c.Result)
		}
		jsonEqual(t, c.Name+"/"+c.Verdict+" lead", map[string]any{
			"email": lead["email"], "email_confidence": lead["email_confidence"], "email_provider": lead["email_provider"]}, c.Lead)
	}
}

func (r DeliverabilityResult) wire() DeliverabilityResult {
	if r.VerificationAttempts == nil {
		r.VerificationAttempts = []Attempt{}
	}
	return r
}

// ---- waterfall -------------------------------------------------------------

type scriptedFinder struct {
	name   string
	email  string
	extra  map[string]string
	errMsg string
	avail  bool
}

func (f *scriptedFinder) Name() string                                       { return f.name }
func (f *scriptedFinder) Available(context.Context, string) bool            { return f.avail }
func (f *scriptedFinder) Find(context.Context, string, map[string]any, string) (string, map[string]string, error) {
	if f.errMsg != "" {
		return "", nil, errors.New(f.errMsg)
	}
	return f.email, f.extra, nil
}

func TestWaterfallVectors(t *testing.T) {
	var v struct {
		WF []struct {
			Name  string `json:"name"`
			Chain []struct {
				Name      string          `json:"name"`
				Spec      json.RawMessage `json:"spec"`
				Available bool            `json:"available"`
			} `json:"chain"`
			LeadIn struct {
				Website   string `json:"website"`
				Email     string `json:"email"`
				Contact   string `json:"contact_person"`
				Waterfall string `json:"enrichment_waterfall"`
			} `json:"lead_in"`
			Outcome json.RawMessage `json:"outcome"`
			LeadOut json.RawMessage `json:"lead_out"`
		} `json:"find_email_waterfall"`
		Usable []struct {
			Email  *string `json:"email"`
			Usable bool    `json:"usable"`
		} `json:"is_usable_email"`
		Domains []struct {
			URL    string `json:"url"`
			Domain string `json:"domain"`
		} `json:"domain_from_url"`
	}
	loadJSON(t, "waterfall_vectors.json", &v)
	if len(v.WF) != 13 || len(v.Usable) != 24 || len(v.Domains) != 11 {
		t.Fatalf("vectors: %d %d %d", len(v.WF), len(v.Usable), len(v.Domains))
	}
	for _, c := range v.WF {
		t.Run(c.Name, func(t *testing.T) {
			d := NewDeps()
			for _, ch := range c.Chain {
				f := &scriptedFinder{name: ch.Name, avail: ch.Available}
				var raises struct {
					Raises string `json:"raises"`
					Len    int    `json:"len"`
				}
				if json.Unmarshal(ch.Spec, &raises) == nil && raises.Raises != "" {
					f.errMsg = strings.Repeat("x", raises.Len)
				} else {
					var arr []json.RawMessage
					_ = json.Unmarshal(ch.Spec, &arr)
					_ = json.Unmarshal(arr[0], &f.email)
					_ = json.Unmarshal(arr[1], &f.extra)
				}
				d.Finders = append(d.Finders, f)
			}
			lead := map[string]any{"company": "Acme", "website": c.LeadIn.Website, "email": c.LeadIn.Email,
				"contact_person": c.LeadIn.Contact, "enrichment_waterfall": c.LeadIn.Waterfall}
			res, err := Waterfall(context.Background(), d, "ws1", lead)
			if err != nil {
				t.Fatal(err)
			}
			jsonEqual(t, "outcome", res, c.Outcome)
			var wf any
			if s, _ := lead["enrichment_waterfall"].(string); s != "" {
				if err := json.Unmarshal([]byte(s), &wf); err != nil {
					t.Fatalf("enrichment_waterfall is not JSON: %v", err)
				}
			}
			str := func(k string) string { s, _ := lead[k].(string); return s }
			attempts, _ := lead["enrichment_attempts"].(int)
			jsonEqual(t, "lead_out", map[string]any{
				"email": str("email"), "email_provider": str("email_provider"), "contact_person": str("contact_person"),
				"contact_title": str("contact_title"), "enrichment_attempts": attempts, "enrichment_waterfall": wf}, c.LeadOut)
		})
	}
	for _, c := range v.Usable {
		e := ""
		if c.Email != nil {
			e = *c.Email
		}
		if got := IsUsableEmail(e); got != c.Usable {
			t.Errorf("IsUsableEmail(%q) = %v, want %v", e, got, c.Usable)
		}
	}
	for _, c := range v.Domains {
		if got := DomainFromURL(c.URL); got != c.Domain {
			t.Errorf("DomainFromURL(%q) = %q, want %q", c.URL, got, c.Domain)
		}
	}
}
