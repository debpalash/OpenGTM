package destdeliver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/airtable"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/instantly"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/intkit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/sheets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/integrations/smartlead"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Live delivery ports for the plan 07 destination types (hubspot, salesforce, instantly, smartlead, google_sheets,
// airtable), built on the same integration and CRM clients the workbook output columns use (Python
// destinations/engine._deliver called the same services/integrations functions).
//
// Credentials are the destination workspace's own secrets only: a global setting or process environment value
// reads as unset, so an editor who types a campaign, spreadsheet, base or CRM target can never write with the
// operator's key (post-port rule; Python fell back to both). All traffic goes through LiveConfig.HTTP, which in
// production is the SSRF-guarded egress client; vendor error text is scrubbed of the credential by the clients.
//
// Rate limits: a vendor 429 (Instantly, Smartlead, Sheets, Airtable) is retried up to RateLimitRetries times with
// exponential backoff (1 s, 2 s, 4 s) inside the delivery timeout; Python failed the member at once. A 429 means the
// request was not applied, and every call is an idempotent upsert or a campaign add the vendor dedupes, so a retry
// cannot duplicate. The CRM clients already retry 429 honouring Retry-After (crm.RequestWithRetry).

// RateLimitRetries is the number of extra attempts after a vendor 429.
const RateLimitRetries = 3

// LiveConfig wires NewLivePorts.
type LiveConfig struct {
	Pool    *pgxpool.Pool     // CRM row stamping (crmpush; destinations never stamp a workbook row)
	Secrets *secrets.Resolver // nil: no credential resolves (every delivery fails "not connected")
	HTTP    *http.Client      // the SSRF-guarded egress client
	Log     *slog.Logger
	// Sleep waits between 429 retries; nil honours ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewLivePorts returns Ports with the CRM, campaign, sheet and Airtable seams filled; Secrets is the resolver
// itself (webhook and ad lookups keep their own policies). Webhook/Slack are wired by the caller.
func NewLivePorts(c LiveConfig) Ports {
	var s intkit.Secrets = wsSecrets{r: c.Secrets}
	l := &live{sleep: c.Sleep}
	if l.sleep == nil {
		l.sleep = ctxSleep
	}
	p := Ports{
		CRM:       crmLive{r: c.Secrets, d: crmpush.Deps{HubSpot: &crm.HubSpot{HTTP: c.HTTP, Secrets: c.Secrets}, Salesforce: &crm.Salesforce{HTTP: c.HTTP, Secrets: c.Secrets}, Pool: c.Pool, Log: c.Log}},
		Instantly: instantlyLive{live: l, c: instantly.New(c.HTTP, s)},
		Smartlead: smartleadLive{live: l, c: smartlead.New(c.HTTP, s)},
		Sheets:    sheetsLive{live: l, c: sheets.New(c.HTTP, s)},
		Airtable:  airtableLive{live: l, c: airtable.New(c.HTTP, s)},
	}
	if c.Secrets != nil { // never a typed-nil resolver behind the interface
		p.Secrets = c.Secrets
	}
	return p
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// wsSecrets is an intkit.Secrets that answers only with the workspace's own value.
type wsSecrets struct{ r *secrets.Resolver }

func (w wsSecrets) Get(ctx context.Context, ws, key string) (string, error) {
	v, ok, err := workspaceSecret(ctx, w.r, ws, key)
	if err != nil || !ok {
		return "", err
	}
	return v, nil
}

func workspaceSecret(ctx context.Context, r *secrets.Resolver, ws, key string) (string, bool, error) {
	if r == nil || ws == "" {
		return "", false, nil
	}
	v, src, err := r.Get(ctx, ws, key)
	if err != nil || v == "" || src != secrets.SourceWorkspace {
		return "", false, err
	}
	return v, true, nil
}

// crmLive is crmpush.Push behind a workspace-credential check: the CRM clients walk workspace > global > env
// themselves, so the port requires the workspace's own keys first and otherwise answers Python's not-connected text
// without a request.
type crmLive struct {
	r *secrets.Resolver
	d crmpush.Deps
}

var crmKeys = map[string]struct {
	label string
	keys  []string
}{
	"hubspot":    {"HubSpot", []string{"HUBSPOT_TOKEN"}},
	"salesforce": {"Salesforce", []string{"SALESFORCE_INSTANCE_URL", "SALESFORCE_ACCESS_TOKEN"}},
}

func (c crmLive) Push(ctx context.Context, ws string, cfg, lead map[string]any, wb string, leadID int64) crmpush.Result {
	if k, ok := crmKeys[crmpush.ParseConfig(cfg).Type]; ok {
		for _, key := range k.keys {
			if _, found, err := workspaceSecret(ctx, c.r, ws, key); err != nil || !found {
				return crmpush.Result{Error: k.label + " not connected"}
			}
		}
	}
	return crmpush.Push(ctx, c.d, ws, cfg, lead, wb, leadID)
}

// live holds the shared 429 policy of the four vendor adapters.
type live struct {
	sleep func(context.Context, time.Duration) error
}

// call runs one vendor call, retrying a 429 failure with backoff. The last result stands when the retries or the
// context run out.
func (l *live) call(ctx context.Context, fn func() (*intkit.Obj, error)) (*intkit.Obj, error) {
	delay := time.Second
	for attempt := 0; ; attempt++ {
		res, err := fn()
		if err != nil || attempt >= RateLimitRetries || !rateLimited(res) {
			return res, err
		}
		if l.sleep(ctx, delay) != nil {
			return res, nil
		}
		delay *= 2
	}
}

// deliver is call followed by result.
func (l *live) deliver(ctx context.Context, externalKey string, fn func() (*intkit.Obj, error)) (Result, error) {
	o, err := l.call(ctx, fn)
	return result(o, err, externalKey)
}

func rateLimited(o *intkit.Obj) bool {
	if o == nil || objBool(o, "success") {
		return false
	}
	return strings.HasPrefix(objStr(o, "error"), "HTTP 429:")
}

// result maps a client's {"success", "error", "duplicate", "operation", <external>} object onto Result. A nil
// object with a nil error cannot happen; it reads as a failure.
func result(o *intkit.Obj, err error, externalKey string) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	if o == nil {
		return Result{Error: "no result"}, nil
	}
	r := Result{Success: objBool(o, "success"), Error: objStr(o, "error"), Duplicate: objBool(o, "duplicate"), Operation: objStr(o, "operation")}
	if externalKey != "" {
		r.ExternalID = objStr(o, externalKey)
	}
	return r, nil
}

func objBool(o *intkit.Obj, k string) bool {
	v, _ := o.Get(k)
	return pyTruthy(v)
}

// objStr is str(value) for a present non-None value, else "".
func objStr(o *intkit.Obj, k string) string {
	v, ok := o.Get(k)
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return colgraph.PyStr(v)
}

// toObj converts a decoded payload into the clients' ordered object, recursively (nested custom_variables /
// custom_fields must be objects for the clients to merge them). Keys are sorted: Python sent the field-map order,
// which only changes byte order, never the JSON the vendor reads.
func toObj(m map[string]any) *intkit.Obj {
	o := wbcrud.NewObj()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, toObjValue(m[k]))
	}
	return o
}

func toObjValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return toObj(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toObjValue(e)
		}
		return out
	case json.Number:
		return x
	}
	return v
}

func cfgStr(cfg map[string]any, k string) string {
	s, _ := cfg[k].(string)
	return s
}

type instantlyLive struct {
	*live
	c *instantly.Client
}

func (a instantlyLive) AddLead(ctx context.Context, r CampaignRequest) (Result, error) {
	skip, _ := r.Config["skip_if_in_campaign"].(bool)
	lead := toObj(r.Payload)
	return a.deliver(ctx, "lead_id", func() (*intkit.Obj, error) {
		return a.c.AddLeadToCampaign(ctx, r.WorkspaceID, cfgStr(r.Config, "campaign_id"), lead, skip)
	})
}

type smartleadLive struct {
	*live
	c *smartlead.Client
}

// AddLead sends config.settings when it is a non-empty object (Python sent any truthy value; the config validator
// only admits an object).
func (a smartleadLive) AddLead(ctx context.Context, r CampaignRequest) (Result, error) {
	var settings *intkit.Obj
	if m, ok := r.Config["settings"].(map[string]any); ok && len(m) > 0 {
		settings = toObj(m)
	}
	lead := toObj(r.Payload)
	return a.deliver(ctx, "", func() (*intkit.Obj, error) {
		return a.c.AddLeadToCampaign(ctx, r.WorkspaceID, cfgStr(r.Config, "campaign_id"), lead, settings)
	})
}

type sheetsLive struct {
	*live
	c *sheets.Client
}

func (a sheetsLive) Upsert(ctx context.Context, r RowUpsert) (Result, error) {
	row, _ := r.Config["row"].([]any)
	values := make([]any, len(row))
	for i, v := range row {
		values[i] = toObjValue(v)
	}
	return a.deliver(ctx, "range", func() (*intkit.Obj, error) {
		return a.c.UpsertRow(ctx, r.WorkspaceID, cfgStr(r.Config, "spreadsheet_id"), values, cfgStr(r.Config, "key"), cfgStr(r.Config, "range"))
	})
}

type airtableLive struct {
	*live
	c *airtable.Client
}

func (a airtableLive) Upsert(ctx context.Context, r RowUpsert) (Result, error) {
	typecast, _ := r.Config["typecast"].(bool)
	fields := toObj(r.Payload)
	return a.deliver(ctx, "record_id", func() (*intkit.Obj, error) {
		return a.c.UpsertRecord(ctx, r.WorkspaceID, fields, cfgStr(r.Config, "base_id"), cfgStr(r.Config, "table"),
			cfgStr(r.Config, "key"), cfgStr(r.Config, "idempotency_field"), typecast)
	})
}
