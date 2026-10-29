package api

import (
	"context"
	"net/http"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provaccuracy"
)

// providerAccuracyNow is the clock of the freshness decay (nil: the wall clock); tests freeze it.
var providerAccuracyNow func() time.Time

// GET /api/workbooks/meta/provider-accuracy[?persist=true]: the per-provider correctness ranking from the offline
// eval harness. persist=true also writes the prior onto provider_stats. Deviation from Python: persist=true needs
// editor/admin (tables.write); Python let any caller write the shared ledger.
func (h *llmRoutes) providerAccuracy(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	persist := v.optBool("persist")
	if len(v.errs) > 0 {
		httpx.ValidationError(w, v.errs...)
		return
	}
	if persist != nil && *persist {
		persistGuard.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accuracyHandlerKey{}, h)))
		return
	}
	h.accuracyRun(w, r, false)
}

type accuracyHandlerKey struct{}

// persistGuard puts the role check in front of the write (identity.RequireRole must run after Workspace, which the
// route's own middleware chain already did).
var persistGuard = identity.RequireRole("tables.write", "editor", "admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	r.Context().Value(accuracyHandlerKey{}).(*llmRoutes).accuracyRun(w, r, true)
}))

func (h *llmRoutes) accuracyRun(w http.ResponseWriter, r *http.Request, persist bool) {
	now := time.Now()
	if providerAccuracyNow != nil {
		now = providerAccuracyNow()
	}
	golden, outputs, err := provaccuracy.Dataset()
	if err != nil {
		h.internal(w, "provider-accuracy dataset", err)
		return
	}
	scores := provaccuracy.ScoreOutputs(golden, outputs, now)
	if persist {
		tx, err := h.d.Pool.Begin(r.Context())
		if err != nil {
			h.internal(w, "provider-accuracy persist", err)
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		if _, err := provaccuracy.Persist(r.Context(), tx, scores, now); err != nil {
			h.internal(w, "provider-accuracy persist", err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			h.internal(w, "provider-accuracy persist", err)
			return
		}
	}
	writePy(w, map[string]any{"ranking": provaccuracy.Rows(provaccuracy.Rank(scores)), "persisted": persist})
}
