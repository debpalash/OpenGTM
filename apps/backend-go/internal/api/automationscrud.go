package api

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// registerAutomationsCRUD registers the seven rule routes of routers/automations.py: create, list, get, patch,
// delete, pause and resume. Mutations are admin routes (automations.manage), reads need only workspace
// membership. Like FastAPI the order is authentication, workspace, role (403), request validation (422), and only
// then the AUTOMATIONS_ENABLED check (404 "automations disabled").
func registerAutomationsCRUD(r *httpx.Router, d Deps) {
	h := newAutoCRUD(d)
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	admin := identity.RequireRole("automations.manage", "admin")
	r.Handle("POST /api/automations/triggers", http.HandlerFunc(h.create), authn, workspace, admin)
	r.Handle("GET /api/automations/triggers", http.HandlerFunc(h.list), authn, workspace)
	r.Handle("GET /api/automations/triggers/{trigger_id}", http.HandlerFunc(h.get), authn, workspace)
	r.Handle("PATCH /api/automations/triggers/{trigger_id}", http.HandlerFunc(h.patch), authn, workspace, admin)
	r.Handle("DELETE /api/automations/triggers/{trigger_id}", http.HandlerFunc(h.remove), authn, workspace, admin)
	r.Handle("POST /api/automations/triggers/{trigger_id}/pause", http.HandlerFunc(h.pause), authn, workspace, admin)
	r.Handle("POST /api/automations/triggers/{trigger_id}/resume", http.HandlerFunc(h.resume), authn, workspace, admin)
}

type autoCRUD struct {
	d   Deps
	svc *autosvc.Service
}

func newAutoCRUD(d Deps) autoCRUD {
	a := d.Flags.Automations
	return autoCRUD{d: d, svc: &autosvc.Service{
		Pool: d.Pool,
		Cfg: autosvc.Config{
			MaxRulesPerWS:       a.MaxRulesPerWS,
			MaxActionsPerRule:   a.MaxActionsPerRule,
			AllowLegacyOutreach: d.Flags.AutomationsAllowLegacyOutreach,
			WebhookAllowlist:    a.WebhookDomainAllowlist,
			Secrets:             secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), d.getenv()),
		},
		ExecFor: d.automationExecutor(),
		Log:     d.Log,
	}}
}

func (h autoCRUD) ws(r *http.Request) identity.WorkspaceCtx {
	wc, _ := identity.WorkspaceFrom(r.Context())
	return wc
}

// enabled answers 404 "automations disabled" when AUTOMATIONS_ENABLED is off.
func (h autoCRUD) enabled(w http.ResponseWriter) bool {
	if h.d.Flags.AutomationsEnabled {
		return true
	}
	httpx.Detail(w, http.StatusNotFound, "automations disabled")
	return false
}

// fail renders a service error: rule errors keep their status and detail, anything else is a 500 that never echoes
// the error text.
func (h autoCRUD) fail(w http.ResponseWriter, what string, err error) {
	var he *autosvc.HTTPError
	if errors.As(err, &he) {
		httpx.WriteJSON(w, he.Status, map[string]any{"detail": he.Detail})
		return
	}
	l := h.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("automations: "+what+" failed", "err_type", fmt.Sprintf("%T", err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h autoCRUD) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, ok := wbcrud.ReadBody(r.Body)
	if !ok {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
	}
	return raw, ok
}

// ruleID is the path id. Ids are opaque strings; one Postgres cannot store (NUL, invalid UTF-8) can match no rule.
func (h autoCRUD) ruleID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("trigger_id")
	if strings.ContainsRune(id, 0) || !utf8.ValidString(id) {
		httpx.Detail(w, http.StatusNotFound, "trigger not found")
		return "", false
	}
	return id, true
}

func (h autoCRUD) create(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, errs := crudParseCreate(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	if !h.enabled(w) {
		return
	}
	wc := h.ws(r)
	in.CreatedBy = strconv.Itoa(int(wc.User.ID))
	t, err := h.svc.Create(r.Context(), wc.WorkspaceID, in)
	if err != nil {
		h.fail(w, "create", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (h autoCRUD) list(w http.ResponseWriter, r *http.Request) {
	var f autosvc.ListFilter
	var errs []httpx.FieldError
	if vals, present := r.URL.Query()["enabled"]; present && len(vals) > 0 {
		raw := vals[len(vals)-1]
		if b, ok := parseBoolQuery(raw); ok {
			f.Enabled = &b
		} else {
			errs = append(errs, httpx.FieldError{Type: "bool_parsing", Loc: []any{"query", "enabled"}, Input: raw,
				Msg: "Input should be a valid boolean, unable to interpret input"})
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	if vals := r.URL.Query()["trigger_type"]; len(vals) > 0 {
		f.TriggerType = vals[len(vals)-1]
	}
	if !h.enabled(w) {
		return
	}
	out, err := h.svc.List(r.Context(), h.ws(r).WorkspaceID, f)
	if err != nil {
		h.fail(w, "list", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h autoCRUD) get(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w) {
		return
	}
	id, ok := h.ruleID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), h.ws(r).WorkspaceID, id)
	if err != nil {
		h.fail(w, "get", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h autoCRUD) patch(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, errs := crudParsePatch(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	if !h.enabled(w) {
		return
	}
	id, ok := h.ruleID(w, r)
	if !ok {
		return
	}
	t, err := h.svc.Patch(r.Context(), h.ws(r).WorkspaceID, id, in)
	if err != nil {
		h.fail(w, "patch", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (h autoCRUD) remove(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w) {
		return
	}
	id, ok := h.ruleID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), h.ws(r).WorkspaceID, id); err != nil {
		h.fail(w, "delete", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (h autoCRUD) pause(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w) {
		return
	}
	id, ok := h.ruleID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Pause(r.Context(), h.ws(r).WorkspaceID, id); err != nil {
		h.fail(w, "pause", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

func (h autoCRUD) resume(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w) {
		return
	}
	id, ok := h.ruleID(w, r)
	if !ok {
		return
	}
	next, err := h.svc.Resume(r.Context(), h.ws(r).WorkspaceID, id)
	if err != nil {
		h.fail(w, "resume", err)
		return
	}
	var nextRun *string
	if next != nil {
		s := autosvc.Iso(*next)
		nextRun = &s
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Enabled   bool    `json:"enabled"`
		NextRunAt *string `json:"next_run_at"`
	}{true, nextRun})
}

// ---- request models (TriggerCreate, TriggerPatch) ---------------------------------------------------------------

const crudMaxInt32 = 1<<31 - 1

func crudErr(typ, msg string, loc []any, in any, ctx map[string]any) httpx.FieldError {
	return httpx.FieldError{Type: typ, Loc: loc, Msg: msg, Input: in, Ctx: ctx}
}

func crudLoc(rest ...any) []any { return append([]any{"body"}, rest...) }

// crudParseCreate validates a TriggerCreate body the way FastAPI/pydantic does (field order, messages, locs), plus the
// D4 bounds: caps must be finite, non-negative and fit the integer column.
func crudParseCreate(raw []byte) (autosvc.RuleInput, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return autosvc.RuleInput{}, errs
	}
	in := autosvc.RuleInput{TriggerConfig: wbcrud.NewObj(), Enabled: true}
	add := func(e ...httpx.FieldError) { errs = append(errs, e...) }

	if v, ok := obj.Get("name"); !ok {
		add(crudErr("missing", "Field required", crudLoc("name"), obj, nil))
	} else if s, e := crudStr(v, crudLoc("name")); e != nil {
		add(*e)
	} else if n := utf8.RuneCountInString(s); n < 1 {
		add(crudErr("string_too_short", "String should have at least 1 character", crudLoc("name"), v, map[string]any{"min_length": 1}))
	} else if n > 200 {
		add(crudErr("string_too_long", "String should have at most 200 characters", crudLoc("name"), v, map[string]any{"max_length": 200}))
	} else {
		in.Name = s
	}
	if v, ok := obj.Get("trigger_type"); !ok {
		add(crudErr("missing", "Field required", crudLoc("trigger_type"), obj, nil))
	} else if s, e := crudStr(v, crudLoc("trigger_type")); e != nil {
		add(*e)
	} else {
		in.TriggerType = &s
	}
	if v, ok := obj.Get("trigger_config"); ok {
		if o, isObj := v.(*wbcrud.Obj); isObj {
			in.TriggerConfig = o
		} else {
			add(crudErr("dict_type", "Input should be a valid dictionary", crudLoc("trigger_config"), v, nil))
		}
	}
	if v, ok := obj.Get("condition"); ok {
		if s, e := crudStr(v, crudLoc("condition")); e != nil {
			add(*e)
		} else {
			in.Condition = s
		}
	}
	if v, ok := obj.Get("actions"); ok {
		if a, e := crudActions(v); e != nil {
			add(e...)
		} else {
			in.Actions = a
		}
	}
	if v, ok := obj.Get("scope_workbook_ids"); ok {
		if a, e := crudStrList(v, "scope_workbook_ids"); e != nil {
			add(e...)
		} else {
			in.ScopeWorkbookIDs = a
		}
	}
	if v, ok := obj.Get("stop_on_error"); ok {
		if b, e := crudBool(v, crudLoc("stop_on_error")); e != nil {
			add(*e)
		} else {
			in.StopOnError = b
		}
	}
	if v, ok := obj.Get("max_spend_usd_per_day"); ok && v != nil {
		if f, e := crudCapFloat(v, crudLoc("max_spend_usd_per_day")); e != nil {
			add(*e)
		} else {
			in.MaxSpendUSDPerDay = &f
		}
	}
	if v, ok := obj.Get("max_actions_per_day"); ok && v != nil {
		if n, e := crudCapInt(v, crudLoc("max_actions_per_day")); e != nil {
			add(*e)
		} else {
			in.MaxActionsPerDay = &n
		}
	}
	if v, ok := obj.Get("enabled"); ok {
		if b, e := crudBool(v, crudLoc("enabled")); e != nil {
			add(*e)
		} else {
			in.Enabled = b
		}
	}
	if len(errs) > 0 {
		return autosvc.RuleInput{}, errs
	}
	return in, nil
}

// crudParsePatch validates a TriggerPatch body: every field is optional and null means unchanged.
func crudParsePatch(raw []byte) (autosvc.PatchInput, []httpx.FieldError) {
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return autosvc.PatchInput{}, errs
	}
	var in autosvc.PatchInput
	add := func(e ...httpx.FieldError) { errs = append(errs, e...) }
	present := func(k string) (any, bool) {
		v, ok := obj.Get(k)
		return v, ok && v != nil
	}

	if v, ok := present("name"); ok {
		if s, e := crudStr(v, crudLoc("name")); e != nil {
			add(*e)
		} else {
			in.Name = &s
		}
	}
	if v, ok := present("trigger_config"); ok {
		if o, isObj := v.(*wbcrud.Obj); isObj {
			in.TriggerConfig = o
		} else {
			add(crudErr("dict_type", "Input should be a valid dictionary", crudLoc("trigger_config"), v, nil))
		}
	}
	if v, ok := present("condition"); ok {
		if s, e := crudStr(v, crudLoc("condition")); e != nil {
			add(*e)
		} else {
			in.Condition = &s
		}
	}
	if v, ok := present("actions"); ok {
		if a, e := crudActions(v); e != nil {
			add(e...)
		} else {
			in.Actions = &a
		}
	}
	if v, ok := present("scope_workbook_ids"); ok {
		if a, e := crudStrList(v, "scope_workbook_ids"); e != nil {
			add(e...)
		} else {
			in.ScopeWorkbookIDs = &a
		}
	}
	if v, ok := present("stop_on_error"); ok {
		if b, e := crudBool(v, crudLoc("stop_on_error")); e != nil {
			add(*e)
		} else {
			in.StopOnError = &b
		}
	}
	if v, ok := present("max_spend_usd_per_day"); ok {
		if f, e := crudCapFloat(v, crudLoc("max_spend_usd_per_day")); e != nil {
			add(*e)
		} else {
			in.MaxSpendUSDPerDay = &f
		}
	}
	if v, ok := present("max_actions_per_day"); ok {
		if n, e := crudCapInt(v, crudLoc("max_actions_per_day")); e != nil {
			add(*e)
		} else {
			in.MaxActionsPerDay = &n
		}
	}
	if v, ok := present("enabled"); ok {
		if b, e := crudBool(v, crudLoc("enabled")); e != nil {
			add(*e)
		} else {
			in.Enabled = &b
		}
	}
	if len(errs) > 0 {
		return autosvc.PatchInput{}, errs
	}
	return in, nil
}

func crudStr(v any, loc []any) (string, *httpx.FieldError) {
	s, ok := v.(string)
	if !ok {
		e := crudErr("string_type", "Input should be a valid string", loc, v, nil)
		return "", &e
	}
	return s, nil
}

// crudActions is list[dict]: a non-list is list_type, every non-object element dict_type at its index.
func crudActions(v any) ([]any, []httpx.FieldError) {
	list, ok := v.([]any)
	if !ok {
		return nil, []httpx.FieldError{crudErr("list_type", "Input should be a valid list", crudLoc("actions"), v, nil)}
	}
	var errs []httpx.FieldError
	out := make([]any, 0, len(list))
	for i, e := range list {
		if o, isObj := e.(*wbcrud.Obj); isObj {
			out = append(out, o)
		} else {
			errs = append(errs, crudErr("dict_type", "Input should be a valid dictionary", crudLoc("actions", i), e, nil))
		}
	}
	if errs != nil {
		return nil, errs
	}
	return out, nil
}

func crudStrList(v any, field string) ([]string, []httpx.FieldError) {
	list, ok := v.([]any)
	if !ok {
		return nil, []httpx.FieldError{crudErr("list_type", "Input should be a valid list", crudLoc(field), v, nil)}
	}
	var errs []httpx.FieldError
	out := make([]string, 0, len(list))
	for i, e := range list {
		if s, isStr := e.(string); isStr {
			out = append(out, s)
		} else {
			errs = append(errs, crudErr("string_type", "Input should be a valid string", crudLoc(field, i), e, nil))
		}
	}
	if errs != nil {
		return nil, errs
	}
	return out, nil
}

// crudBool is pydantic's lax bool.
func crudBool(v any, loc []any) (bool, *httpx.FieldError) {
	bad := crudErr("bool_parsing", "Input should be a valid boolean, unable to interpret input", loc, v, nil)
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(t) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return false, &bad
	case interface{ String() string }: // json.Number
		f, err := strconv.ParseFloat(t.String(), 64)
		switch {
		case err == nil && f == 0:
			return false, nil
		case err == nil && f == 1:
			return true, nil
		case err == nil && f == math.Trunc(f):
			return false, &bad
		}
	}
	e := crudErr("bool_type", "Input should be a valid boolean", loc, v, nil)
	return false, &e
}

// crudEchoText is the input echoed in an error: an out-of-range number as its text, so the 422 body stays parseable.
func crudEchoText(v any) any {
	if n, ok := v.(interface{ String() string }); ok {
		return n.String()
	}
	return v
}

// crudCapFloat is Optional[float] with the D4 bounds: finite and >= 0.
func crudCapFloat(v any, loc []any) (float64, *httpx.FieldError) {
	var f float64
	switch t := v.(type) {
	case bool:
		if t {
			f = 1
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(t), 64); err != nil && !errors.Is(err, strconv.ErrRange) {
			e := crudErr("float_parsing", "Input should be a valid number, unable to parse string as a number", loc, v, nil)
			return 0, &e
		}
	case interface{ String() string }: // json.Number
		var err error
		if f, err = strconv.ParseFloat(t.String(), 64); err != nil && !errors.Is(err, strconv.ErrRange) {
			e := crudErr("float_parsing", "Input should be a valid number, unable to parse string as a number", loc, v, nil)
			return 0, &e
		}
	default:
		e := crudErr("float_type", "Input should be a valid number", loc, v, nil)
		return 0, &e
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		e := crudErr("finite_number", "Input should be a finite number", loc, crudEchoText(v), nil)
		return 0, &e
	}
	if f < 0 {
		e := crudErr("greater_than_equal", "Input should be greater than or equal to 0", loc, v, map[string]any{"ge": 0})
		return 0, &e
	}
	return f, nil
}

// crudCapInt is Optional[int] with the D4 bounds: 0 to the largest int4.
func crudCapInt(v any, loc []any) (int32, *httpx.FieldError) {
	var n *big.Int
	switch t := v.(type) {
	case bool:
		n = big.NewInt(0)
		if t {
			n = big.NewInt(1)
		}
	case string, interface{ String() string }:
		text := ""
		if s, ok := t.(string); ok {
			text = strings.TrimSpace(s)
		} else {
			text = t.(interface{ String() string }).String()
		}
		_, isStr := t.(string)
		if i, ok := new(big.Int).SetString(text, 10); ok {
			n = i
			break
		}
		f, err := strconv.ParseFloat(text, 64)
		switch {
		case err != nil && !errors.Is(err, strconv.ErrRange) || (isStr && (math.IsNaN(f) || math.IsInf(f, 0))):
			e := crudErr("int_parsing", "Input should be a valid integer, unable to parse string as an integer", loc, v, nil)
			return 0, &e
		case math.IsNaN(f) || math.IsInf(f, 0):
			e := crudErr("finite_number", "Input should be a finite number", loc, crudEchoText(v), nil)
			return 0, &e
		case f != math.Trunc(f):
			msg := "Input should be a valid integer, got a number with a fractional part"
			if isStr {
				msg = "Input should be a valid integer, unable to parse string as an integer"
			}
			typ := "int_from_float"
			if isStr {
				typ = "int_parsing"
			}
			e := crudErr(typ, msg, loc, v, nil)
			return 0, &e
		}
		n, _ = big.NewFloat(f).Int(nil)
	default:
		e := crudErr("int_type", "Input should be a valid integer", loc, v, nil)
		return 0, &e
	}
	if n.Sign() < 0 {
		e := crudErr("greater_than_equal", "Input should be greater than or equal to 0", loc, v, map[string]any{"ge": 0})
		return 0, &e
	}
	if !n.IsInt64() || n.Int64() > crudMaxInt32 {
		e := crudErr("less_than_equal", "Input should be less than or equal to 2147483647", loc, v, map[string]any{"le": crudMaxInt32})
		return 0, &e
	}
	return int32(n.Int64()), nil
}
