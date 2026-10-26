package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

const maxSettingsBody = 1 << 20

// RegisterSettings registers /api/settings/providers. Every route needs an
// active user; PUT and POST .../test also need a global admin.
func RegisterSettings(r *httpx.Router, d Deps) {
	reg := d.Registry
	if reg == nil || d.Envelope == nil || d.Egress == nil {
		return
	}
	app := &settings.App{
		Store: secrets.NewPgStore(d.Pool), Env: d.Envelope,
		Secret: reg.SecretSet(), Getenv: d.getenv(),
	}
	s := settingsRoutes{
		p:      &settings.Providers{Reg: reg, App: app, HTTP: d.Egress, Log: d.Log},
		perf:   &settings.Perf{App: app, Reg: reg},
		models: &settings.Models{Reg: reg, HTTP: d.Egress, OpenRouterURL: d.OpenRouterModelsURL},
		enrich: &settings.EnrichmentProviders{Reg: reg, App: app, Log: d.Log},
		usage:  &settings.LLMUsage{Reg: reg, Pool: d.Pool},
		app:    app,
		getenv: d.getenv(),
		log:    d.Log,
	}
	integ := integrationRoutes{settingsRoutes: s, st: d.Identity, integ: &settings.Integrations{
		List: reg.Integrations, App: app, WS: secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), d.getenv()),
	}}
	user := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity)}
	admin := append(append([]httpx.Middleware{}, user...), identity.RequireGlobalAdmin())
	r.Handle("GET /api/settings/providers", http.HandlerFunc(s.list), user...)
	r.Handle("PUT /api/settings/providers/{provider_id}", http.HandlerFunc(s.update), admin...)
	r.Handle("POST /api/settings/providers/{provider_id}/test", http.HandlerFunc(s.test), admin...)
	r.Handle("GET /api/settings/enrichment-perf", http.HandlerFunc(s.perfGet), user...)
	r.Handle("PUT /api/settings/enrichment-perf", http.HandlerFunc(s.perfPut), admin...)
	r.Handle("GET /api/settings/system", http.HandlerFunc(s.system), user...)
	r.Handle("GET /api/settings/models/{provider_id}", http.HandlerFunc(s.modelsList), user...)
	r.Handle("GET /api/settings/enrichment-providers", http.HandlerFunc(s.enrichList), user...)
	r.Handle("PUT /api/settings/enrichment-providers/{provider_id}", http.HandlerFunc(s.enrichUpdate), admin...)
	r.Handle("GET /api/settings/integrations", http.HandlerFunc(integ.list), user...)
	r.Handle("PUT /api/settings/integrations/{integration_id}", http.HandlerFunc(integ.update), admin...)
	r.Handle("GET /api/settings/workspace-integrations", http.HandlerFunc(integ.wsList), user...)
	r.Handle("PUT /api/settings/workspace-integrations/{integration_id}", http.HandlerFunc(integ.wsUpdate), user...)
	ws := append(append([]httpx.Middleware{}, user...), identity.Workspace(d.Identity))
	r.Handle("GET /api/settings/llm-usage", http.HandlerFunc(s.llmUsage), ws...)
}

type settingsRoutes struct {
	p      *settings.Providers
	perf   *settings.Perf
	models *settings.Models
	enrich *settings.EnrichmentProviders
	usage  *settings.LLMUsage
	app    *settings.App
	getenv func(string) string
	log    *slog.Logger
}

func (s settingsRoutes) fail(w http.ResponseWriter, what string) {
	l := s.log
	if l == nil {
		l = slog.Default()
	}
	l.Error("settings providers: " + what + " failed")
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (s settingsRoutes) list(w http.ResponseWriter, r *http.Request) {
	views, def, err := s.p.List(r.Context())
	if err != nil {
		s.fail(w, "list")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"providers": views, "default_provider": def})
}

// decodeBodyObject decodes a required JSON object body like FastAPI does for a
// pydantic model parameter. Numbers stay json.Number so integers are exact.
func decodeBodyObject(raw []byte) (map[string]any, []httpx.FieldError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, []httpx.FieldError{{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil}}
	}
	var body any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil || dec.More() {
		return nil, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return nil, []httpx.FieldError{{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body}}
	}
	return obj, nil
}

// parseProviderUpdate validates the body like pydantic's ProviderUpdate.
func parseProviderUpdate(raw []byte) (settings.ProviderUpdate, []httpx.FieldError) {
	var u settings.ProviderUpdate
	obj, errs := decodeBodyObject(raw)
	if errs != nil {
		return u, errs
	}
	str := func(name string) *string {
		v, present := obj[name]
		if !present || v == nil {
			return nil
		}
		s, isStr := v.(string)
		if !isStr {
			errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", name}, Msg: "Input should be a valid string", Input: v})
			return nil
		}
		return &s
	}
	u.APIKey, u.BaseURL, u.Model = str("api_key"), str("base_url"), str("model")
	if v, present := obj["set_default"]; present && v != nil {
		b, isBool := v.(bool)
		if !isBool {
			errs = append(errs, httpx.FieldError{Type: "bool_type", Loc: []any{"body", "set_default"}, Msg: "Input should be a valid boolean", Input: v})
		}
		u.SetDefault = b
	}
	return u, errs
}

func (s settingsRoutes) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider_id")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSettingsBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	// FastAPI validates the body before the handler checks the provider id.
	u, errs := parseProviderUpdate(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	found, err := s.p.Update(r.Context(), id, u)
	if !found {
		httpx.Detail(w, http.StatusNotFound, "Provider '"+id+"' not found")
		return
	}
	if err != nil {
		s.fail(w, "update")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "provider": id})
}

func (s settingsRoutes) test(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider_id")
	res, found, err := s.p.Test(r.Context(), id)
	if !found {
		httpx.Detail(w, http.StatusNotFound, "Provider '"+id+"' not found")
		return
	}
	if err != nil {
		s.fail(w, "test")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// parseLaxInt validates one optional Optional[int] field like pydantic v2 in
// lax mode. A nil result with no error means "not provided" (absent or null).
func parseLaxInt(obj map[string]any, name string) (*int64, *httpx.FieldError) {
	v, present := obj[name]
	if !present || v == nil {
		return nil, nil
	}
	loc := []any{"body", name}
	bad := func(typ, msg string) (*int64, *httpx.FieldError) {
		return nil, &httpx.FieldError{Type: typ, Loc: loc, Msg: msg, Input: v}
	}
	var text string
	switch t := v.(type) {
	case bool:
		n := int64(0)
		if t {
			n = 1
		}
		return &n, nil
	case json.Number:
		text = t.String()
	case string:
		text = strings.TrimSpace(t)
	default:
		return bad("int_type", "Input should be a valid integer")
	}
	_, isStr := v.(string)
	r, ok := new(big.Rat).SetString(text)
	if !ok || (isStr && strings.ContainsAny(text, "/")) {
		if isStr {
			return bad("int_parsing", "Input should be a valid integer, unable to parse string as an integer")
		}
		return bad("int_type", "Input should be a valid integer")
	}
	if !r.IsInt() {
		if isStr {
			return bad("int_parsing", "Input should be a valid integer, unable to parse string as an integer")
		}
		return bad("int_from_float", "Input should be a valid integer, got a number with a fractional part")
	}
	// Out-of-range values saturate: the route clamps to small bounds anyway.
	n := int64(math.MaxInt64)
	if r.Num().IsInt64() {
		n = r.Num().Int64()
	} else if r.Sign() < 0 {
		n = math.MinInt64
	}
	return &n, nil
}

func parsePerfUpdate(raw []byte) (settings.PerfUpdate, []httpx.FieldError) {
	var u settings.PerfUpdate
	obj, errs := decodeBodyObject(raw)
	if errs != nil {
		return u, errs
	}
	for _, f := range []struct {
		name string
		dst  **int64
	}{
		{"row_concurrency", &u.RowConcurrency}, {"provider_workers", &u.ProviderWorkers},
		{"provider_timeout", &u.ProviderTimeout}, {"max_providers", &u.MaxProviders}, {"retry_passes", &u.RetryPasses},
	} {
		n, fe := parseLaxInt(obj, f.name)
		if fe != nil {
			errs = append(errs, *fe)
			continue
		}
		*f.dst = n
	}
	return u, errs
}

func (s settingsRoutes) perfGet(w http.ResponseWriter, r *http.Request) {
	out, err := s.perf.Get(r.Context())
	if err != nil {
		s.fail(w, "perf get")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s settingsRoutes) perfPut(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSettingsBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	u, errs := parsePerfUpdate(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	out, err := s.perf.Put(r.Context(), u)
	if err != nil {
		s.fail(w, "perf put")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s settingsRoutes) system(w http.ResponseWriter, r *http.Request) {
	out, err := settings.SystemInfo(r.Context(), s.app, s.getenv)
	if err != nil {
		s.fail(w, "system")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// parseBoolQuery is pydantic's lax bool for query strings.
func parseBoolQuery(v string) (val, ok bool) {
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y", "on":
		return true, true
	case "0", "false", "f", "no", "n", "off":
		return false, true
	}
	return false, false
}

func (s settingsRoutes) modelsList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider_id")
	free := false
	if q, present := r.URL.Query()["free_only"]; present {
		var ok bool
		if free, ok = parseBoolQuery(q[0]); !ok {
			httpx.ValidationError(w, httpx.FieldError{Type: "bool_parsing", Loc: []any{"query", "free_only"},
				Msg: "Input should be a valid boolean, unable to interpret input", Input: q[0]})
			return
		}
	}
	body, found := s.models.List(r.Context(), id, free)
	if !found {
		httpx.Detail(w, http.StatusNotFound, "Provider '"+id+"' not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

func (s settingsRoutes) enrichList(w http.ResponseWriter, r *http.Request) {
	views, err := s.enrich.List(r.Context())
	if err != nil {
		s.fail(w, "enrichment list")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"providers": views})
}

func (s settingsRoutes) enrichUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider_id")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSettingsBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	u, errs := parseProviderUpdate(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	found, err := s.enrich.Update(r.Context(), id, u)
	if !found {
		httpx.Detail(w, http.StatusNotFound, "Enrichment provider '"+id+"' not found")
		return
	}
	if err != nil {
		s.fail(w, "enrichment update")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "provider": id})
}

func (s settingsRoutes) llmUsage(w http.ResponseWriter, r *http.Request) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		s.fail(w, "llm usage context")
		return
	}
	out, err := s.usage.Get(r.Context(), c.WorkspaceID, r.URL.Query().Get("date"))
	if err != nil {
		s.fail(w, "llm usage")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
