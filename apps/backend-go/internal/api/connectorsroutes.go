package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/certification"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/connectors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Environment names for the manifest directory and trust store. The defaults are the
// Python locations relative to the repository root (MANIFESTS_DIR, TRUST_STORE).
const (
	connectorManifestsEnv  = "OPENGTM_CONNECTOR_MANIFESTS_DIR"
	connectorTrustStoreEnv = "OPENGTM_CONNECTOR_TRUST_STORE"
	connectorPolicyEnv     = "CONNECTOR_SIGNATURE_POLICY"

	defaultConnectorManifests = "legacy/python/apps/api/services/leadgen/enrichment/declarative/manifests"
	defaultConnectorTrust     = "docs/connectors/trusted-publishers.json"
)

// connectorsNow is the clock for certification expiry (tests pin it; nil = time.Now).
var connectorsNow func() time.Time

type connectorRoutes struct {
	deps      connectors.Deps
	trustPath string
	log       *slog.Logger
}

// current returns the deps with the trust store re-read, as Python does per request.
func (h connectorRoutes) current() connectors.Deps {
	d := h.deps
	d.Trust = manifest.LoadTrustStore(h.trustPath)
	return d
}

// registerConnectors registers the connectors route group (routers/connectors.py): both
// routes sit behind current_workspace only.
func registerConnectors(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Envelope == nil {
		return
	}
	getenv := d.getenv()
	dir, trust := getenv(connectorManifestsEnv), getenv(connectorTrustStoreEnv)
	if dir == "" {
		dir = defaultConnectorManifests
	}
	if trust == "" {
		trust = defaultConnectorTrust
	}
	raw := getenv(connectorPolicyEnv)
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	h := connectorRoutes{log: log, trustPath: trust, deps: connectors.Deps{
		Dir:     dir,
		Policy:  connectors.EffectivePolicy(raw, raw != ""),
		Secrets: secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), getenv),
		Cert: certification.Options{
			Path: d.Flags.CertificationPath, Key: d.Flags.CertificationKey.Reveal(),
			BuildSHA: d.Flags.BuildSHA, Now: connectorsNow,
		},
	}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	r.Handle("GET /api/connectors/catalog", http.HandlerFunc(h.catalog), authn, workspace)
	r.Handle("GET /api/connectors/compatibility", http.HandlerFunc(h.compatibility), authn, workspace)
}

func (h connectorRoutes) catalog(w http.ResponseWriter, r *http.Request) {
	ctx, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	out, err := connectors.Catalog(r.Context(), h.current(), ctx.WorkspaceID)
	switch {
	case errors.Is(err, connectors.ErrUnknownConnectorSubject):
		httpx.Detail(w, connectors.UnknownSubjectStatus, "Connector manifest name is invalid")
	case errors.Is(err, context.Canceled):
		return
	case err != nil:
		h.log.Error("connector catalog failed", "err", err)
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	default:
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

func (h connectorRoutes) compatibility(w http.ResponseWriter, r *http.Request) {
	// The workspace dependency is intentional (as Python): validation output is only for
	// an authenticated workspace member.
	d := h.current()
	httpx.WriteJSON(w, http.StatusOK, connectors.Compatibility(connectors.ValidateDirectory(d.Dir, d.Policy, d.Trust)))
}
