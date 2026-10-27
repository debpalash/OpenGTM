package api

import (
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/watchsvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const watchCursorMaxLen = 1024

type watchCRUDRoutes struct {
	d   Deps
	svc *watchsvc.Service
}

// watchPollExecutor is go_executor.watch_poll_executor: "go" only when GO_EXECUTOR_ENABLED,
// GO_EXECUTOR_WATCH_POLL and the workspace is in GO_EXECUTOR_WORKSPACES (read per call so a flag
// change applies at once, like automationExecutor).
func (d Deps) watchPollExecutor() func(ws string) string {
	return func(ws string) string {
		c := config.ExecutorCohortFrom(d.getenv())
		return automationhook.CohortExecutor(c.Enabled, d.Flags.Automations.GoExecutorWatchPoll, c.Workspaces)(ws)
	}
}

// watchCRUDService builds the watch store behind the CRUD routes from the loaded settings. Webhook
// secret references resolve inside the caller's workspace only (secrets.Resolver.Has).
func (d Deps) watchCRUDService() *watchsvc.Service {
	a := d.Flags.Automations
	var registered func(string) bool
	if d.Registry != nil {
		registered = d.Registry.SecretSet()
	}
	return &watchsvc.Service{
		Pool: d.Pool,
		Cfg: watchsvc.Config{
			DefaultInterval:       a.PollerDefaultInterval,
			MaxWatchesPerWS:       a.PollerMaxWatchesPerWS,
			JobChangeMaxContacts:  a.PollerJobChangeMaxContacts,
			PollNowMinIntervalSec: a.PollerPollNowMinIntervalSec,
			PollNowDailyQuota:     a.PollerPollNowDailyQuota,
			AutomationsEnabled:    d.Flags.AutomationsEnabled,
			WebhookAllowlist:      a.WebhookDomainAllowlist,
		},
		Secrets: secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), d.getenv()),
		// Same outbound secret-ref policy as automation rules (S-M1): env prefixes plus the registered-credential deny-list.
		Outbound: secrets.OutboundPolicy{Prefixes: config.OutboundSecretRefPrefixes(d.getenv()), Registered: registered},
		ExecFor:  d.watchPollExecutor(),
		Log:      d.logger(),
	}
}

// registerWatchesCRUD registers POST/GET /api/watches and GET/PATCH/DELETE /api/watches/{watch_id}.
// Mutations sit behind require_editor (signals.write: editor, admin), reads behind current_workspace.
// Dependencies run first (401/403), then body/query validation (422), then _require_enabled (404
// poller disabled, 409 lead store), then the service.
func registerWatchesCRUD(r *httpx.Router, d Deps) {
	h := watchCRUDRoutes{d: d, svc: d.watchCRUDService()}
	authn := identity.Authenticate(d.Signer, d.Identity)
	read := []httpx.Middleware{authn, identity.Workspace(d.Identity)}
	write := []httpx.Middleware{authn, identity.Workspace(d.Identity), identity.RequireRole("signals.write", "editor", "admin")}
	r.Handle("POST /api/watches", http.HandlerFunc(h.create), write...)
	r.Handle("GET /api/watches", http.HandlerFunc(h.list), read...)
	r.Handle("GET /api/watches/{watch_id}", http.HandlerFunc(h.get), read...)
	r.Handle("PATCH /api/watches/{watch_id}", http.HandlerFunc(h.patch), write...)
	r.Handle("DELETE /api/watches/{watch_id}", http.HandlerFunc(h.remove), write...)
}

// gated is _require_enabled: false after answering 404 or 409.
func (h watchCRUDRoutes) gated(w http.ResponseWriter) bool {
	if e := watchsvc.RequireEnabled(h.d.Flags.IntentPollerEnabled, h.d.Flags.PGLeadStore); e != nil {
		h.fail(w, e)
		return false
	}
	return true
}

// fail answers a service error: HTTPError as FastAPI's {"detail": ...}, anything else as an opaque 500
// (the error text may carry row data, so it is never echoed or logged).
func (h watchCRUDRoutes) fail(w http.ResponseWriter, err error) {
	var he *watchsvc.HTTPError
	if errors.As(err, &he) {
		if s, ok := he.Detail.(string); ok {
			httpx.Detail(w, he.Status, s)
			return
		}
		httpx.WriteJSON(w, he.Status, map[string]any{"detail": he.Detail})
		return
	}
	h.d.logger().Error("watches: request failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h watchCRUDRoutes) ws(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return "", false
	}
	return c.WorkspaceID, true
}

// requireAutomationsManage gates the rule a watch create can attach: the
// automation routes need automations.manage (admin), so an editor must not get
// the same rule through the watches API (Go hardening; Python lets it through).
func (h watchCRUDRoutes) requireAutomationsManage(w http.ResponseWriter, r *http.Request) bool {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return false
	}
	var allowed bool
	err := c.Tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		allowed, err = identity.HasPermissionTx(r.Context(), tx, c.WorkspaceID, c.User.ID, "automations.manage", "admin")
		return err
	})
	if err != nil {
		h.d.logger().Error("watches: permission check failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return false
	}
	if !allowed {
		httpx.Detail(w, http.StatusForbidden, "Workspace permission denied: automations.manage")
		return false
	}
	return true
}

// readObject reads and decodes a pydantic-model body (ordered, exact numbers).
func watchBody(w http.ResponseWriter, r *http.Request) (*wbcrud.Obj, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, wbcrud.MaxBodyBytes+1))
	if err != nil || len(raw) > wbcrud.MaxBodyBytes {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return nil, false
	}
	return obj, true
}

func watchFE(typ, msg string, loc []any, in any) httpx.FieldError {
	return httpx.FieldError{Type: typ, Loc: loc, Msg: msg, Input: in}
}

// watchStr is Optional[str] / str: a JSON string (pydantic does not coerce numbers to str).
func watchStr(v any, loc []any) (string, *httpx.FieldError) {
	s, ok := v.(string)
	if !ok {
		e := watchFE("string_type", "Input should be a valid string", loc, v)
		return "", &e
	}
	return s, nil
}

// watchInt is pydantic's lax int: integers, integral floats, bools and numeric strings.
func watchInt(v any, loc []any) (int64, *httpx.FieldError) {
	parsing := func() (int64, *httpx.FieldError) {
		e := watchFE("int_parsing", "Input should be a valid integer, unable to parse string as an integer", loc, v)
		return 0, &e
	}
	switch t := v.(type) {
	case interface{ String() string }: // json.Number
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return parsing()
			}
			return n, nil
		}
		f, _ := strconv.ParseFloat(s, 64)
		if f != math.Trunc(f) || math.IsInf(f, 0) {
			e := watchFE("int_from_float", "Input should be a valid integer, got a number with a fractional part", loc, v)
			return 0, &e
		}
		if math.Abs(f) >= 1<<63 {
			return parsing()
		}
		return int64(f), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) && math.Abs(f) < 1<<63 {
			return int64(f), nil
		}
		return parsing()
	}
	e := watchFE("int_type", "Input should be a valid integer", loc, v)
	return 0, &e
}

// watchBool is pydantic's lax bool: bool, 0/1 and the usual spellings.
func watchBool(v any, loc []any) (bool, *httpx.FieldError) {
	bad := watchFE("bool_parsing", "Input should be a valid boolean, unable to interpret input", loc, v)
	switch t := v.(type) {
	case bool:
		return t, nil
	case interface{ String() string }:
		switch f, _ := strconv.ParseFloat(t.String(), 64); f {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
		return false, &bad
	case string:
		switch strings.ToLower(t) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return false, &bad
	}
	e := watchFE("bool_type", "Input should be a valid boolean", loc, v)
	return false, &e
}

// watchStrList is Optional[list[str]].
func watchStrList(v any, loc []any) ([]string, []httpx.FieldError) {
	l, ok := v.([]any)
	if !ok {
		return nil, []httpx.FieldError{watchFE("list_type", "Input should be a valid list", loc, v)}
	}
	out := make([]string, 0, len(l))
	var errs []httpx.FieldError
	for i, x := range l {
		s, e := watchStr(x, append(append([]any{}, loc...), i))
		if e != nil {
			errs = append(errs, *e)
			continue
		}
		out = append(out, s)
	}
	return out, errs
}

// watchDict is Optional[dict]: any JSON object, kept ordered.
func watchDict(v any, loc []any) (*wbcrud.Obj, *httpx.FieldError) {
	o, ok := v.(*wbcrud.Obj)
	if !ok {
		e := watchFE("dict_type", "Input should be a valid dictionary", loc, v)
		return nil, &e
	}
	return o, nil
}

func watchLeadID(n int64) (*int32, error) {
	if n < math.MinInt32 || n > math.MaxInt32 {
		// a lead id outside the integer column can never exist (Python: DB error, 500)
		return nil, &watchsvc.HTTPError{Status: http.StatusUnprocessableEntity, Detail: "lead_id " + strconv.FormatInt(n, 10) + " not found in this workspace"}
	}
	v := int32(n)
	return &v, nil
}

// parseWatchCreate validates WatchCreate. leadErr is a deferred 422 (out-of-range lead id) raised after
// the flag gate, where Python reaches the database.
func parseWatchCreate(obj *wbcrud.Obj) (in watchsvc.CreateInput, leadErr error, errs []httpx.FieldError) {
	body := []any{"body"}
	at := func(k string) []any { return append(append([]any{}, body...), k) }
	add := func(e *httpx.FieldError) {
		if e != nil {
			errs = append(errs, *e)
		}
	}
	if v, ok := obj.Get("kind"); !ok {
		errs = append(errs, watchFE("missing", "Field required", at("kind"), obj))
	} else if s, e := watchStr(v, at("kind")); e != nil {
		add(e)
	} else {
		in.Kind = s
	}
	if v, ok := obj.Get("target"); !ok {
		errs = append(errs, watchFE("missing", "Field required", at("target"), obj))
	} else if s, e := watchStr(v, at("target")); e != nil {
		add(e)
	} else if utf8.RuneCountInString(s) < 1 {
		errs = append(errs, httpx.FieldError{Type: "string_too_short", Loc: at("target"), Input: v,
			Msg: "String should have at least 1 character", Ctx: map[string]any{"min_length": 1}})
	} else {
		in.Target = s
	}
	if v, ok := obj.Get("lead_id"); ok && v != nil {
		if n, e := watchInt(v, at("lead_id")); e != nil {
			add(e)
		} else {
			in.LeadID, leadErr = watchLeadID(n)
		}
	}
	if v, ok := obj.Get("signal_types"); ok && v != nil {
		l, e := watchStrList(v, at("signal_types"))
		errs = append(errs, e...)
		in.SignalTypes = l
	}
	if v, ok := obj.Get("interval"); ok && v != nil {
		if s, e := watchStr(v, at("interval")); e != nil {
			add(e)
		} else {
			in.Interval = &s
		}
	}
	if v, ok := obj.Get("config"); ok && v != nil {
		if o, e := watchDict(v, at("config")); e != nil {
			add(e)
		} else {
			in.Config = o
		}
	}
	if v, ok := obj.Get("create_webhook_rule"); ok {
		if b, e := watchBool(v, at("create_webhook_rule")); e != nil {
			add(e)
		} else {
			in.CreateWebhookRule = b
		}
	}
	for _, f := range []struct {
		key string
		dst **string
	}{{"webhook_url", &in.WebhookURL}, {"webhook_secret_ref", &in.WebhookSecretRef}} {
		if v, ok := obj.Get(f.key); ok && v != nil {
			if s, e := watchStr(v, at(f.key)); e != nil {
				add(e)
			} else {
				*f.dst = &s
			}
		}
	}
	return in, leadErr, errs
}

// parseWatchPatch validates WatchPatch (every field optional; null means unchanged).
func parseWatchPatch(obj *wbcrud.Obj) (in watchsvc.PatchInput, leadErr error, errs []httpx.FieldError) {
	at := func(k string) []any { return []any{"body", k} }
	add := func(e *httpx.FieldError) {
		if e != nil {
			errs = append(errs, *e)
		}
	}
	if v, ok := obj.Get("enabled"); ok && v != nil {
		if b, e := watchBool(v, at("enabled")); e != nil {
			add(e)
		} else {
			in.Enabled = &b
		}
	}
	if v, ok := obj.Get("interval"); ok && v != nil {
		if s, e := watchStr(v, at("interval")); e != nil {
			add(e)
		} else {
			in.Interval = &s
		}
	}
	if v, ok := obj.Get("signal_types"); ok && v != nil {
		l, e := watchStrList(v, at("signal_types"))
		errs = append(errs, e...)
		if len(e) == 0 {
			in.SignalTypes = &l
		}
	}
	if v, ok := obj.Get("lead_id"); ok && v != nil {
		if n, e := watchInt(v, at("lead_id")); e != nil {
			add(e)
		} else {
			in.LeadID, leadErr = watchLeadID(n)
		}
	}
	if v, ok := obj.Get("config"); ok && v != nil {
		if o, e := watchDict(v, at("config")); e != nil {
			add(e)
		} else {
			in.Config = o
		}
	}
	return in, leadErr, errs
}

func (h watchCRUDRoutes) create(w http.ResponseWriter, r *http.Request) {
	obj, ok := watchBody(w, r)
	if !ok {
		return
	}
	in, leadErr, errs := parseWatchCreate(obj)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	if !h.gated(w) {
		return
	}
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	if leadErr != nil {
		h.fail(w, leadErr)
		return
	}
	if in.CreateWebhookRule && !h.requireAutomationsManage(w, r) {
		return
	}
	out, err := h.svc.Create(r.Context(), ws, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h watchCRUDRoutes) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var errs []httpx.FieldError
	one, five, zero := int64(1), int64(500), int64(0)
	def100 := int64(100)
	limit, e := wbcrud.QueryInt(q, "limit", &def100, &one, &five)
	if e != nil {
		errs = append(errs, *e)
	}
	offset, e := wbcrud.QueryInt(q, "offset", &zero, &zero, nil)
	if e != nil {
		errs = append(errs, *e)
	}
	cursor := watchLastQuery(q, "cursor")
	if utf8.RuneCountInString(cursor) > watchCursorMaxLen {
		errs = append(errs, httpx.FieldError{Type: "string_too_long", Loc: []any{"query", "cursor"}, Input: cursor,
			Msg: "String should have at most 1024 characters", Ctx: map[string]any{"max_length": watchCursorMaxLen}})
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	if !h.gated(w) {
		return
	}
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	out, err := h.svc.List(r.Context(), ws, int(limit), int(offset), cursor)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func watchLastQuery(q url.Values, k string) string {
	if v := q[k]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (h watchCRUDRoutes) get(w http.ResponseWriter, r *http.Request) {
	if !h.gated(w) {
		return
	}
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), ws, r.PathValue("watch_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h watchCRUDRoutes) patch(w http.ResponseWriter, r *http.Request) {
	obj, ok := watchBody(w, r)
	if !ok {
		return
	}
	in, leadErr, errs := parseWatchPatch(obj)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	if !h.gated(w) {
		return
	}
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	if leadErr != nil {
		// Python loads the watch first: an unknown watch is 404 before the lead pin is judged.
		if _, err := h.svc.Get(r.Context(), ws, r.PathValue("watch_id")); err != nil {
			h.fail(w, err)
			return
		}
		h.fail(w, leadErr)
		return
	}
	out, err := h.svc.Patch(r.Context(), ws, r.PathValue("watch_id"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h watchCRUDRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if !h.gated(w) {
		return
	}
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	id := r.PathValue("watch_id")
	if err := h.svc.Delete(r.Context(), ws, id); err != nil {
		h.fail(w, err)
		return
	}
	o := wbcrud.NewObj()
	o.Set("deleted", true)
	o.Set("id", id)
	httpx.WriteJSON(w, http.StatusOK, o)
}
