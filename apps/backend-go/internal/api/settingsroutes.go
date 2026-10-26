package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

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
	s := settingsRoutes{
		p: &settings.Providers{
			Reg: reg,
			App: &settings.App{
				Store: secrets.NewPgStore(d.Pool), Env: d.Envelope,
				Secret: reg.SecretSet(), Getenv: d.getenv(),
			},
			HTTP: d.Egress, Log: d.Log,
		},
		log: d.Log,
	}
	user := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity)}
	admin := append(append([]httpx.Middleware{}, user...), identity.RequireGlobalAdmin())
	r.Handle("GET /api/settings/providers", http.HandlerFunc(s.list), user...)
	r.Handle("PUT /api/settings/providers/{provider_id}", http.HandlerFunc(s.update), admin...)
	r.Handle("POST /api/settings/providers/{provider_id}/test", http.HandlerFunc(s.test), admin...)
}

type settingsRoutes struct {
	p   *settings.Providers
	log *slog.Logger
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

// parseProviderUpdate validates the body like pydantic's ProviderUpdate.
func parseProviderUpdate(raw []byte) (settings.ProviderUpdate, []httpx.FieldError) {
	var u settings.ProviderUpdate
	if len(bytes.TrimSpace(raw)) == 0 {
		return u, []httpx.FieldError{{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil}}
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return u, []httpx.FieldError{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return u, []httpx.FieldError{{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body}}
	}
	var errs []httpx.FieldError
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
