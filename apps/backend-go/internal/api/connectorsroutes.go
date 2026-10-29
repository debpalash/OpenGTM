package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/certification"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/connectors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest/builtin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Environment names for the manifest directory and trust store. When unset (or empty)
// the manifests and trust store embedded in the binary (internal/manifest/builtin, byte
// copies of the Python MANIFESTS_DIR and TRUST_STORE) are used; an override is read from disk.
const (
	connectorManifestsEnv  = "OPENGTM_CONNECTOR_MANIFESTS_DIR"
	connectorTrustStoreEnv = "OPENGTM_CONNECTOR_TRUST_STORE"
	connectorPolicyEnv     = "CONNECTOR_SIGNATURE_POLICY"
)

// connectorSources resolves the manifest directory and trust store file: the env overrides
// win, otherwise the embedded built-ins (materialised once to a private temp directory so the
// loaders see ordinary files). A materialisation failure is logged and yields a missing
// path, i.e. an empty catalog, never a crash.
func connectorSources(lookup func(string) (string, bool)) (dir, trust string) {
	dir, _ = lookup(connectorManifestsEnv)
	trust, _ = lookup(connectorTrustStoreEnv)
	if dir != "" && trust != "" {
		return dir, trust
	}
	bd, bt, err := builtin.Paths()
	if err != nil {
		slog.Default().Error("connector built-ins unavailable", "err", err)
		bd, bt = "builtin-unavailable/manifests", "builtin-unavailable/trusted-publishers.json"
	}
	if dir == "" {
		dir = bd
	}
	if trust == "" {
		trust = bt
	}
	return dir, trust
}

// connectorsNow is the clock for certification expiry (tests pin it; nil = time.Now).
var connectorsNow func() time.Time

// connectorsLookupEnv reads the connector env settings with presence (a set-but-empty
// CONNECTOR_SIGNATURE_POLICY is "required", as Python); tests replace it.
var connectorsLookupEnv = os.LookupEnv

type connectorRoutes struct {
	deps      connectors.Deps
	certKey   config.Secret // revealed per request, never held as plaintext
	trustPath string
	log       *slog.Logger
}

// current returns the deps with the trust store re-read, as Python does per request.
func (h connectorRoutes) current() connectors.Deps {
	d := h.deps
	d.Trust = manifest.LoadTrustStore(h.trustPath)
	d.Cert.Key = h.certKey.Reveal()
	return d
}

// registerConnectors registers the connectors route group (routers/connectors.py): both
// routes sit behind current_workspace only.
func registerConnectors(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Envelope == nil {
		return
	}
	getenv := d.getenv()
	dir, trust := connectorSources(connectorsLookupEnv)
	raw, policySet := connectorsLookupEnv(connectorPolicyEnv)
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	h := connectorRoutes{log: log, trustPath: trust, certKey: d.Flags.CertificationKey, deps: connectors.Deps{
		Dir:     dir,
		Policy:  connectors.EffectivePolicy(raw, policySet),
		Secrets: secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), getenv),
		Cert: certification.Options{
			Path:     d.Flags.CertificationPath,
			BuildSHA: d.Flags.BuildSHA, Now: connectorsNow,
		},
	}}
	// Paths only: an override pointing nowhere would otherwise yield a silently empty catalog.
	log.Info("connector catalog sources", "manifests_dir", dir, "trust_store", trust, "signature_policy", h.deps.Policy)
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
