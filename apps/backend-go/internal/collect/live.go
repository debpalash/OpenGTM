package collect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect/scrapers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcecatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcehealth"
)

// Production adapters of the Runner's ports.

// LiveWeb is Web over internal/leadweb for one workspace (the search cache is salted with it).
type LiveWeb struct {
	W  *leadweb.Web
	WS string
}

func (l LiveWeb) Search(ctx context.Context, q string, n int) []leadweb.Hit {
	return l.W.Search.Text(ctx, q, leadweb.SearchOptions{MaxResults: n, Workspace: l.WS})
}

func (l LiveWeb) Fetch(ctx context.Context, url string, tier int, timeout time.Duration) (string, bool) {
	r := l.W.Fetch.Fetch(ctx, url, leadweb.FetchOptions{Tier: tier, Timeout: timeout})
	return r.Text, r.OK()
}

// LiveDiscovery is Discovery over the browser service.
type LiveDiscovery struct{ B scrapers.Browser }

func (d LiveDiscovery) Maps(ctx context.Context, q, city string, n int) ([]scrapers.Place, error) {
	if d.B == nil {
		return nil, browserclient.ErrUnavailable
	}
	return scrapers.GoogleMaps(ctx, d.B, q, city, n, "")
}

// DecisionMakers is the job's find_decision_makers(company, None, 3) fallback; like legacy it
// returns every title's hit (max_results was never applied there).
func (d LiveDiscovery) DecisionMakers(ctx context.Context, company string, _ int) ([]scrapers.Person, error) {
	if d.B == nil {
		return nil, browserclient.ErrUnavailable
	}
	return scrapers.FindDecisionMakers(ctx, d.B, company, nil, 0, "")
}

// LiveSources reads the operator switches from the global settings and the health gate from
// source_health.
type LiveSources struct {
	Settings sourcecatalog.Getter
	Pool     *pgxpool.Pool
	Enforce  bool // SOURCE_HEALTH_ENFORCE
}

func (s LiveSources) Enabled(ctx context.Context, id string) bool {
	return sourcecatalog.Enabled(ctx, s.Settings, id)
}

// HealthDisabled is fail-open: any error is no exclusion.
func (s LiveSources) HealthDisabled(ctx context.Context) map[string]bool {
	if !s.Enforce || s.Pool == nil {
		return nil
	}
	var out map[string]bool
	if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = sourcehealth.DisabledNames(ctx, tx, true)
		return err
	}); err != nil {
		return nil
	}
	return out
}

// ProviderCaller is the in-process provider caller (rpclocal.Caller).
type ProviderCaller interface {
	Call(ctx context.Context, call provcall.Call) (provcall.Result, error)
}

// LiveEnrich runs the email steps in internal/emailverify and the named providers through the
// provider-call caller (in process for the names GO_LOCAL_PROVIDERS lists, else the Python worker).
type LiveEnrich struct {
	Mail    emailverify.Deps
	Caller  ProviderCaller // nil: provider stages find nothing
	Timeout time.Duration  // per provider call (0 = 120 s)
}

func (e LiveEnrich) Waterfall(ctx context.Context, ws string, l *Lead) error {
	m := l.Map()
	if _, err := emailverify.Waterfall(ctx, e.Mail, ws, m); err != nil {
		return err
	}
	l.SetFromMap(m)
	return nil
}

func (e LiveEnrich) Deliverability(ctx context.Context, ws string, l *Lead) (bool, string, bool) {
	m := l.Map()
	res, err := emailverify.TagEmailConfidence(ctx, e.Mail, ws, m)
	if err != nil || res == nil {
		return false, "", false
	}
	l.SetFromMap(m)
	return res.Keep, res.Confidence, true
}

// stageOptions are the provider settings of the collect stages, as JobRunner constructed the
// providers: CrossLinkedProvider(max_people=3, delay=1.5), JobSpySignalProvider(max_jobs=3,
// delay=1.5), MailScoutVerifyProvider(timeout=5, max_verify=2). They travel in the provider-call
// input's reserved "_options" object (rpclocal hands them to the provider as call options).
var stageOptions = map[string]map[string]int{
	"crosslinked": {"max_people": 3, "delay_ms": 1500},
	"jobspy":      {"max_jobs": 3, "delay_ms": 1500},
	"mailscout":   {"max_verify": 2, "timeout_ms": 5000},
}

// ErrNoProviderCaller is returned by Provider when no caller is wired.
var ErrNoProviderCaller = errors.New("collect: no provider caller")

func (e LiveEnrich) Provider(ctx context.Context, ws, name string, l *Lead) (ProviderResult, error) {
	if e.Caller == nil {
		return ProviderResult{}, ErrNoProviderCaller
	}
	m := l.Map()
	if o, ok := stageOptions[name]; ok {
		m["_options"] = o
	}
	input, err := json.Marshal(m)
	if err != nil {
		return ProviderResult{}, err
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	res, err := e.Caller.Call(ctx, provcall.Call{CallID: "collect-" + randHex(12), Provider: name,
		WorkspaceID: ws, Input: input, Timeout: timeout})
	if err != nil {
		return ProviderResult{}, err
	}
	var out struct {
		Success bool           `json:"success"`
		Fields  map[string]any `json:"fields"`
	}
	if len(res.Output) > 0 && json.Unmarshal(res.Output, &out) != nil {
		return ProviderResult{}, errors.New("collect: provider output is not an object")
	}
	return ProviderResult{Success: out.Success, Fields: out.Fields}, nil
}

func (e LiveEnrich) HasMX(ctx context.Context, domain string) bool {
	return emailverify.HasMX(ctx, e.Mail, domain)
}

func (e LiveEnrich) PickBestEmail(ctx context.Context, cands []string) (string, string, *bool) {
	r, err := emailverify.PickBestEmail(ctx, e.Mail, cands)
	if err != nil {
		return "", "", nil
	}
	return r.Email, r.Confidence, r.Deliverable
}

func (e LiveEnrich) VerifyEmail(ctx context.Context, email string) *bool {
	r, err := emailverify.VerifyEmail(ctx, e.Mail, email)
	if err != nil {
		return nil
	}
	return r.Deliverable
}

func (e LiveEnrich) Disposable(email string) bool { return emailverify.Classify(email).IsDisposable }

// BusEmitter publishes to the leadgen progress bus of one workspace.
type BusEmitter struct {
	Bus *leadprogress.Bus
	WS  string
}

func (b BusEmitter) Emit(ctx context.Context, typ string, data leadprogress.Obj) {
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	b.Bus.EmitOrdered(ectx, b.WS, typ, data)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newJobID is uuid.uuid4().hex.
func newJobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}
