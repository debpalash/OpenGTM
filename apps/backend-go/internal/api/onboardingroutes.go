package api

import (
	"context"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/demoseed"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/onboarding"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcecatalog"
)

// RegisterOnboarding registers the first-run checklist: GET /api/onboarding (any member of the workspace) and
// POST /api/onboarding/sample (editor or admin, 20/minute like the other mutating workbook routes). There is no
// DELETE: the sample is an ordinary workbook, so the user deletes it through DELETE /api/workbooks/{id}.
func RegisterOnboarding(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := &onboardingRoutes{d: d, svc: &onboarding.Service{Pool: d.Pool}}
	if d.Registry != nil && d.Envelope != nil {
		app := &settings.App{Store: secrets.NewPgStore(d.Pool), Env: d.Envelope, Secret: d.Registry.SecretSet(), Getenv: d.getenv()}
		prov := &settings.Providers{Reg: d.Registry, App: app, Log: d.Log}
		res := secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), d.getenv())
		h.svc.LLMUsable = func(ctx context.Context, ws string) (bool, error) {
			// Global keys and the keyless custom endpoint come from the provider list (the same "configured"
			// the settings page shows); a key saved for this workspace only is checked per provider.
			views, _, err := prov.List(ctx)
			if err != nil {
				return false, err
			}
			for _, v := range views {
				if v.Configured {
					return true, nil
				}
			}
			for _, id := range d.Registry.Providers.Keys() {
				p, _ := d.Registry.Provider(id)
				if p.EnvKey == "" {
					continue
				}
				ok, err := res.Has(ctx, ws, p.EnvKey)
				if err != nil || ok {
					return ok, err
				}
			}
			return false, nil
		}
		h.svc.SourceEnabled = func(ctx context.Context) (bool, error) {
			for _, src := range sourcecatalog.Sources() {
				if sourcecatalog.Enabled(ctx, app, src.ID) {
					return true, nil
				}
			}
			return false, nil
		}
	} else {
		// Without the settings layer only the catalog defaults can be known.
		h.svc.SourceEnabled = func(ctx context.Context) (bool, error) {
			for _, src := range sourcecatalog.Sources() {
				if sourcecatalog.Enabled(ctx, nil, src.ID) {
					return true, nil
				}
			}
			return false, nil
		}
	}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("tables.write", "editor", "admin")
	r.Handle("GET /api/onboarding", http.HandlerFunc(h.get), authn, workspace)
	mws := []httpx.Middleware{authn, workspace, editor}
	if d.Limiter != nil {
		mws = append(mws, d.Limiter.Limit("20/minute"))
	}
	r.Handle("POST /api/onboarding/sample", http.HandlerFunc(h.sample), mws...)
}

type onboardingRoutes struct {
	d   Deps
	svc *onboarding.Service
}

func (h *onboardingRoutes) wsID(r *http.Request) string {
	wc, _ := identity.WorkspaceFrom(r.Context())
	return wc.WorkspaceID
}

func (h *onboardingRoutes) fail(w http.ResponseWriter, what string, err error) {
	h.d.logger().Error("onboarding: "+what+" failed", "err_type", errType(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h *onboardingRoutes) get(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Get(r.Context(), h.wsID(r))
	if err != nil {
		h.fail(w, "state", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

// sample loads the sample workbook once per workspace: 201 when it was created, 200 with the same body when it
// already existed.
func (h *onboardingRoutes) sample(w http.ResponseWriter, r *http.Request) {
	ws := h.wsID(r)
	res, err := demoseed.LoadSample(r.Context(), h.d.Pool, ws)
	if err != nil {
		h.fail(w, "sample", err)
		return
	}
	audit.SetMetadata(r.Context(), map[string]any{"action": "onboarding.sample.load", "workbook_id": res.WorkbookID, "created": res.Created})
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, map[string]any{"workbook_id": res.WorkbookID, "name": demoseed.SampleName, "created": res.Created})
}
