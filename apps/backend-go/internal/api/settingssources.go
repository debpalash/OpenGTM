package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcecatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcehealth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Test seams over the health layer; nil means the production function.
var (
	srcHealthSummary func(ctx context.Context, tx pgx.Tx, f sourcehealth.Flags) (*sourcehealth.Obj, error)
	srcHealthMap     func(ctx context.Context, tx pgx.Tx) (map[string]*sourcehealth.Obj, error)
	srcManualEnable  func(ctx context.Context, tx pgx.Tx, name string) (*sourcehealth.Obj, error)
	srcHealthReset   func(ctx context.Context, tx pgx.Tx, name string) (*sourcehealth.Obj, error)
)

type sourceSettingsRoutes struct {
	d   Deps
	app *settings.App
}

// registerSourceSettings mounts the 5 /api/settings/sources* routes (routers/settings.py). Every route needs an
// active user (reads are open to any authenticated user, as in Python); PUT and POST .../health/reset need a global
// admin. source_health is a global table without workspace_id or RLS, so a plain transaction is correct. net/http
// prefers the literal /health and /registry paths over {source_id}.
func registerSourceSettings(r *httpx.Router, d Deps) {
	s := &sourceSettingsRoutes{d: d, app: &settings.App{Store: secrets.NewPgStore(d.Pool), Env: d.Envelope, Getenv: d.getenv()}}
	user := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity)}
	admin := append(append([]httpx.Middleware{}, user...), identity.RequireGlobalAdmin())
	r.Handle("GET /api/settings/sources", http.HandlerFunc(s.list), user...)
	r.Handle("GET /api/settings/sources/health", http.HandlerFunc(s.health), user...)
	r.Handle("GET /api/settings/sources/registry", http.HandlerFunc(s.registry), user...)
	r.Handle("PUT /api/settings/sources/{source_id}", http.HandlerFunc(s.toggle), admin...)
	r.Handle("POST /api/settings/sources/{source_id}/health/reset", http.HandlerFunc(s.reset), admin...)
}

// settingGetter reads SOURCE_*_ENABLED. Without an envelope (never in production) values are taken as plaintext:
// the toggles are "0"/"1", never ciphertext.
type settingGetter struct{ s *sourceSettingsRoutes }

func (g settingGetter) Get(ctx context.Context, key, def string) (string, error) {
	if g.s.d.Envelope != nil {
		return g.s.app.Get(ctx, key, def)
	}
	if raw, err := g.s.app.Store.AppSetting(ctx, key); err == nil && raw != "" {
		return raw, nil
	}
	if v := g.s.d.getenv()(key); v != "" {
		return v, nil
	}
	return def, nil
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	}
	return false
}

func (s *sourceSettingsRoutes) flags() sourcehealth.Flags {
	g := s.d.getenv()
	return sourcehealth.Flags{Enforce: truthy(g("SOURCE_HEALTH_ENFORCE")), ProbeEnabled: truthy(g("SOURCE_HEALTH_ENABLED"))}
}

// withTx runs fn in one plain (non-tenant) transaction and commits when fn succeeds.
func (s *sourceSettingsRoutes) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *sourceSettingsRoutes) fail(w http.ResponseWriter, what string, err error) {
	s.d.logger().Error("settings sources: "+what, "err_type", errType(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func errType(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}

func (s *sourceSettingsRoutes) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hmap := map[string]*sourcehealth.Obj{}
	// A health-layer error degrades to null badges (Python's except).
	if err := s.withTx(ctx, func(tx pgx.Tx) error {
		f := srcHealthMap
		if f == nil {
			f = sourcehealth.Map
		}
		m, err := f(ctx, tx)
		if err == nil && m != nil {
			hmap = m
		}
		return err
	}); err != nil {
		hmap = map[string]*sourcehealth.Obj{}
	}
	get := settingGetter{s}
	out := make([]any, 0, len(sourcecatalog.Sources()))
	for _, src := range sourcecatalog.Sources() {
		o := wbcrud.NewObj()
		o.Set("id", src.ID)
		o.Set("name", src.Name)
		o.Set("icon", src.Icon)
		o.Set("description", src.Description)
		o.Set("enabled", sourcecatalog.Enabled(ctx, get, src.ID))
		o.Set("strategy", src.Strategy)
		o.Set("category", src.Category)
		var badge any
		if h := hmap[src.ID]; h != nil {
			b := wbcrud.NewObj()
			for _, k := range []string{"state", "last_yield", "last_ok_at", "disabled_reason", "manual_override"} {
				v, _ := h.Get(k)
				b.Set(k, v)
			}
			badge = b
		}
		o.Set("health", badge)
		out = append(out, o)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *sourceSettingsRoutes) health(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var sum *sourcehealth.Obj
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		f := srcHealthSummary
		if f == nil {
			f = sourcehealth.Summary
		}
		var err error
		sum, err = f(ctx, tx, s.flags())
		return err
	})
	if err != nil || sum == nil {
		// Python answers str(e); the Go body carries a generic message so driver text never leaks.
		o := wbcrud.NewObj()
		o.Set("total", 0)
		o.Set("sources", []any{})
		o.Set("error", "internal error")
		httpx.WriteJSON(w, http.StatusOK, o)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sum)
}

func (s *sourceSettingsRoutes) registry(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, sourcecatalog.Registry())
}

// enabledParam reads `enabled: bool = True`: the last repeated value wins; a blank value is a bool_parsing error.
func enabledParam(r *http.Request) (bool, *httpx.FieldError) {
	vals, ok := r.URL.Query()["enabled"]
	if !ok || len(vals) == 0 {
		return true, nil
	}
	v := vals[len(vals)-1]
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	}
	return false, &httpx.FieldError{Type: "bool_parsing", Loc: []any{"query", "enabled"}, Input: v,
		Msg: "Input should be a valid boolean, unable to interpret input"}
}

func (s *sourceSettingsRoutes) toggle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("source_id")
	on, ferr := enabledParam(r)
	if ferr != nil {
		httpx.ValidationError(w, *ferr)
		return
	}
	known := false
	for _, src := range sourcecatalog.Sources() {
		if src.ID == id {
			known = true
			break
		}
	}
	if !known {
		httpx.Detail(w, http.StatusNotFound, "Source '"+id+"' not found")
		return
	}
	if err := sourcecatalog.SetEnabled(ctx, s.app, id, on); err != nil {
		s.fail(w, "set enabled", err)
		return
	}
	if on {
		// Operator intent is authoritative; the health layer is best-effort and its errors are swallowed.
		_ = s.withTx(ctx, func(tx pgx.Tx) error {
			f := srcManualEnable
			if f == nil {
				f = sourcehealth.ManualEnable
			}
			_, err := f(ctx, tx, id)
			return err
		})
	}
	o := wbcrud.NewObj()
	o.Set("id", id)
	o.Set("enabled", on)
	httpx.WriteJSON(w, http.StatusOK, o)
}

func (s *sourceSettingsRoutes) reset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("source_id")
	var row *sourcehealth.Obj
	if err := s.withTx(ctx, func(tx pgx.Tx) error {
		f := srcHealthReset
		if f == nil {
			f = sourcehealth.Reset
		}
		var err error
		row, err = f(ctx, tx, id)
		return err
	}); err != nil {
		s.fail(w, "reset health", err)
		return
	}
	if row == nil {
		o := wbcrud.NewObj()
		o.Set("name", id)
		o.Set("reset", false)
		o.Set("detail", "no health row")
		httpx.WriteJSON(w, http.StatusOK, o)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row)
}
