package manifest

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Go-only manifest extensions (manifest_version "1" stays valid for manifests that do not use
// them; each is hashed, signed and put in the run payload only when declared):
//
//	auth.secondary        a second credential (key plus secret)
//	response.success_when a body/status condition a response must meet to count as a hit
//	poll                  an asynchronous second request repeated until done or failed

const (
	// MaxPollTotal is the hard ceiling on the wall time one poll sequence may take, whatever the
	// manifest asks for. The caller's context (the worker's per-call timeout) can only shorten it.
	MaxPollTotal = 120 * time.Second
	// DefaultPollTotal applies when poll.max_total is not set.
	DefaultPollTotal = 60 * time.Second
	// MaxPollAttempts is the ceiling on poll requests per lookup.
	MaxPollAttempts = 60
	// MinPollInterval is the shortest wait between poll requests.
	MinPollInterval = 100 * time.Millisecond
	// MaxPollInterval is the longest wait between poll requests.
	MaxPollInterval = 60 * time.Second
)

// SecondaryAuth places the second credential. Type is "header" or "query" and Param names the
// header or query parameter; EnvVar is the environment/settings name that holds the value.
type SecondaryAuth struct {
	Type   string `json:"type"`
	Param  string `json:"param"`
	EnvVar string `json:"env_var"`
}

// Cond is a response condition. Every part that is set must hold: Status (the HTTP status is
// one of the listed codes) and, when Path is set, the value at Path (a "$." path) equals Equals
// or is one of In. Values compare as text: strings as they are, booleans as true/false, numbers
// in their shortest form. A missing, null or empty value never matches.
type Cond struct {
	Path   string `json:"path"`
	Equals any    `json:"equals"`
	In     []any  `json:"in"`
	Status []int  `json:"status"`
}

// PollSpec is the optional second request of an asynchronous lookup. Request is rendered after
// the first response, with {{response.<path>}} reading the first response's JSON. Its URL
// authority is literal and must be the manifest's own host or one of AllowedHosts, so no
// response value can steer a credentialed request to another host.
type PollSpec struct {
	Request      RequestSpec `json:"request"`
	Interval     float64     `json:"interval"`     // seconds waited before every poll request
	MaxAttempts  int         `json:"max_attempts"` // poll requests, 1..MaxPollAttempts
	MaxTotal     float64     `json:"max_total"`    // seconds for the whole sequence, <= MaxPollTotal
	DoneWhen     *Cond       `json:"done_when"`
	FailedWhen   *Cond       `json:"failed_when"`
	AllowedHosts []string    `json:"allowed_hosts"`
}

func intsDoc(in []int) []any {
	out := make([]any, len(in))
	for i, n := range in {
		out[i] = int64(n)
	}
	return out
}

func strsDoc(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// Doc is the condition as a manifest document (only the keys that are set).
func (c Cond) Doc() map[string]any {
	d := map[string]any{}
	if c.Path != "" {
		d["path"] = c.Path
	}
	if c.Equals != nil {
		d["equals"] = c.Equals
	}
	if len(c.In) > 0 {
		d["in"] = append([]any(nil), c.In...)
	}
	if len(c.Status) > 0 {
		d["status"] = intsDoc(c.Status)
	}
	return d
}

// Doc is the secondary credential as a manifest document.
func (s SecondaryAuth) Doc() map[string]any {
	return map[string]any{"type": s.Type, "param": s.Param, "env_var": s.EnvVar}
}

// Doc is the poll block as a manifest document (optional keys only when set).
func (p PollSpec) Doc() map[string]any {
	req := map[string]any{
		"method": p.Request.Method, "url": p.Request.URL, "timeout": p.Request.Timeout,
		"headers": nonNilMap(p.Request.Headers), "query": nonNilMap(p.Request.Query),
		"body_template": p.Request.BodyTemplate,
	}
	if len(p.Request.OmitEmpty) > 0 {
		req["omit_empty"] = strsDoc(p.Request.OmitEmpty)
	}
	d := map[string]any{
		"request": req, "interval": p.Interval, "max_attempts": int64(p.MaxAttempts),
	}
	if p.MaxTotal != 0 {
		d["max_total"] = p.MaxTotal
	}
	if p.DoneWhen != nil {
		d["done_when"] = p.DoneWhen.Doc()
	}
	if p.FailedWhen != nil {
		d["failed_when"] = p.FailedWhen.Doc()
	}
	if len(p.AllowedHosts) > 0 {
		d["allowed_hosts"] = strsDoc(p.AllowedHosts)
	}
	return d
}

func nonNilMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// condText is the text a value is compared as; ok is false for values that never match.
func condText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, x != ""
	case bool:
		return strconv.FormatBool(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case int:
		return strconv.Itoa(x), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false // objects and lists are not comparable
}

// Match reports whether the response (decoded JSON and HTTP status) meets the condition.
func (c Cond) Match(data any, status int) bool {
	if len(c.Status) > 0 {
		ok := false
		for _, s := range c.Status {
			if s == status {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if c.Path == "" {
		return len(c.Status) > 0
	}
	got, ok := condText(ProjectValue(data, c.Path))
	if !ok {
		return false
	}
	if c.Equals != nil {
		want, ok := condText(c.Equals)
		return ok && got == want
	}
	for _, w := range c.In {
		if want, ok := condText(w); ok && got == want {
			return true
		}
	}
	return false
}

// condProblem returns why a condition is unusable, or "".
func condProblem(c *Cond, what string) string {
	if c == nil {
		return ""
	}
	if c.Path == "" && len(c.Status) == 0 {
		return what + " needs a path or a status list"
	}
	if c.Path != "" {
		if !strings.HasPrefix(c.Path, "$.") || len(c.Path) < 3 {
			return what + ".path must start with \"$.\""
		}
		if (c.Equals == nil) == (len(c.In) == 0) {
			return what + " needs exactly one of equals or in"
		}
	} else if c.Equals != nil || len(c.In) > 0 {
		return what + ": equals and in need a path"
	}
	if c.Equals != nil {
		if _, ok := condText(c.Equals); !ok {
			return what + ".equals must be a non-empty string, number or boolean"
		}
	}
	for _, w := range c.In {
		if _, ok := condText(w); !ok {
			return what + ".in must hold non-empty strings, numbers or booleans"
		}
	}
	for _, s := range c.Status {
		if s < 100 || s > 599 {
			return what + ".status must be HTTP status codes"
		}
	}
	return ""
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// extProblem is the refusal reason for the Go-only extensions of m, or "". It is part of
// SecurityProblem, so a manifest that fails it is refused at run time and by the validator.
func extProblem(m Manifest) string {
	a := m.Auth
	if s := a.Secondary; s != nil {
		switch {
		case a.Type == "" || a.Type == "none" || a.EnvVar == "":
			return "secondary_invalid: auth.secondary needs a primary credential"
		case s.Type != "header" && s.Type != "query":
			return "secondary_invalid: auth.secondary.type must be header or query"
		case s.Param == "" || s.EnvVar == "":
			return "secondary_invalid: auth.secondary needs param and env_var"
		case s.EnvVar == a.EnvVar:
			return "secondary_invalid: auth.secondary.env_var repeats auth.env_var"
		case EnvDenied(s.EnvVar):
			return "secondary_env_denied"
		case s.Type == a.Type && strings.EqualFold(s.Param, a.Param):
			return "secondary_invalid: auth.secondary.param repeats auth.param"
		}
	}
	for _, u := range []string{m.Request.URL, pollURL(m)} {
		for _, name := range []string{a.EnvVar, secondaryEnv(a)} {
			if name != "" && strings.Contains(u[:pathEnd(u)], "${env:"+name+"}") {
				return "secret_in_url_path"
			}
		}
	}
	if p := condProblem(m.Response.SuccessWhen, "response.success_when"); p != "" {
		return "success_when_invalid: " + p
	}
	if m.Poll != nil {
		return pollProblem(m)
	}
	return ""
}

// ExtensionProblem is the refusal reason for auth.secondary, response.success_when and poll
// (and for a credential name on the platform deny-list), or "". The connector validator uses it.
func ExtensionProblem(m Manifest) string {
	if m.Auth.EnvVar != "" && EnvDenied(m.Auth.EnvVar) {
		return "env_denied: auth.env_var names a platform secret"
	}
	return extProblem(m)
}

func secondaryEnv(a AuthSpec) string {
	if a.Secondary == nil {
		return ""
	}
	return a.Secondary.EnvVar
}

func pollURL(m Manifest) string {
	if m.Poll == nil {
		return ""
	}
	return m.Poll.Request.URL
}

// pathEnd is the index where the query or fragment of u begins (the credential-free zone is the
// whole URL up to it).
func pathEnd(u string) int {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return i
	}
	return len(u)
}

func pollProblem(m Manifest) string {
	p := m.Poll
	bad := func(s string) string { return "poll_invalid: " + s }
	r := p.Request
	if m.Auth.Type == "" || m.Auth.Type == "none" {
		return bad("poll needs an authenticated manifest (the poll request carries the same credentials)")
	}
	if r.URL == "" {
		return bad("poll.request.url is required")
	}
	if m := strings.ToUpper(r.Method); m != "GET" && m != "POST" {
		return bad("poll.request.method must be GET or POST")
	}
	if strings.Contains(r.URL[:authorityEnd(r.URL)], "{{") || strings.Contains(r.URL[:authorityEnd(r.URL)], "${") {
		return "templated_authority"
	}
	if !(r.Timeout > 0 && r.Timeout <= 120) {
		return bad("poll.request.timeout must be between 0 and 120 seconds")
	}
	if !(p.Interval*float64(time.Second) >= float64(MinPollInterval) && p.Interval*float64(time.Second) <= float64(MaxPollInterval)) {
		return bad("poll.interval must be between 0.1 and 60 seconds")
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > MaxPollAttempts {
		return bad("poll.max_attempts must be between 1 and " + strconv.Itoa(MaxPollAttempts))
	}
	if p.MaxTotal < 0 || p.MaxTotal*float64(time.Second) > float64(MaxPollTotal) {
		return bad("poll.max_total must be between 0 and 120 seconds")
	}
	if p.DoneWhen == nil {
		return bad("poll.done_when is required")
	}
	if s := condProblem(p.DoneWhen, "poll.done_when"); s != "" {
		return bad(s)
	}
	if s := condProblem(p.FailedWhen, "poll.failed_when"); s != "" {
		return bad(s)
	}
	allowed := map[string]bool{hostOf(m.Request.URL): true}
	for _, h := range p.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || strings.ContainsAny(h, "/:@{$*? ") {
			return bad("poll.allowed_hosts entries must be plain host names")
		}
		allowed[h] = true
	}
	if h := hostOf(r.URL); h == "" || !allowed[h] {
		return bad("poll.request.url host must be the request host or listed in poll.allowed_hosts")
	}
	return ""
}

// total is the wall-time cap of one poll sequence.
func (p PollSpec) total() time.Duration {
	t := DefaultPollTotal
	if p.MaxTotal > 0 {
		t = time.Duration(p.MaxTotal * float64(time.Second))
	}
	if t > MaxPollTotal {
		t = MaxPollTotal
	}
	return t
}

// runPoll repeats the poll request until done_when or failed_when holds. It returns the final
// response (data, status) or a Result carrying the error. Every request goes through the same
// guarded client, URL check and credentials as the first one.
func runPoll(ctx context.Context, client *http.Client, m Manifest, p *Prepared, first any, reqs *int) (any, int, string) {
	ps := m.Poll
	pctx, cancel := context.WithTimeout(ctx, ps.total())
	defer cancel()
	allowed := map[string]bool{hostOf(m.Request.URL): true}
	for _, h := range ps.AllowedHosts {
		allowed[strings.ToLower(strings.TrimSpace(h))] = true
	}
	wait := time.Duration(ps.Interval * float64(time.Second))
	for attempt := 1; attempt <= ps.MaxAttempts; attempt++ {
		t := time.NewTimer(wait)
		select {
		case <-pctx.Done():
			t.Stop()
			return nil, 0, pollStop(ctx, pctx)
		case <-t.C:
		}
		pr, perr := p.render(ps.Request, map[string]any{"response": first}, allowed)
		if perr != "" {
			return nil, 0, perr
		}
		rctx := pctx
		if pr.Spec.Timeout > 0 {
			var c context.CancelFunc
			rctx, c = context.WithTimeout(pctx, time.Duration(pr.Spec.Timeout*float64(time.Second)))
			defer c()
		}
		data, status, errStr := p.do(rctx, client, pr)
		*reqs++
		if errStr != "" {
			if pctx.Err() != nil {
				return nil, 0, pollStop(ctx, pctx)
			}
			return nil, status, errStr
		}
		if ps.FailedWhen != nil && ps.FailedWhen.Match(data, status) {
			return data, status, "poll_failed"
		}
		if ps.DoneWhen.Match(data, status) {
			return data, status, ""
		}
	}
	return nil, 0, "poll_max_attempts"
}

// pollStop names why a poll sequence ended early: the caller's own deadline or cancellation, or
// the sequence's hard cap.
func pollStop(parent, poll context.Context) string {
	if parent.Err() != nil {
		return failure(parent.Err())
	}
	return "poll_timeout"
}
