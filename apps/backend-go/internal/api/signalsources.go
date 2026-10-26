package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/certification"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signalscan"
)

// registerSignalSources registers GET /api/signals/sources: the signal-source coverage and
// maturity catalog. Auth as signals.py: the global authenticator plus current_workspace
// (any member, no role gate).
//
// Maturity comes from internal/certification (the fail-closed registry of
// OPENGTM_INTEGRATION_CERTIFICATIONS, signing key and build sha): a source is "supported"
// only with a valid attested controlled-live certificate, otherwise "beta" with
// certification null. The certificate file is re-read per request, like Python.
func registerSignalSources(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	r.Handle("GET /api/signals/sources", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(signalscan.CatalogJSON(certSnapshotFor(d, time.Now)))
	}), authn, workspace)
}

// certSnapshot is a signalscan.Certifier over one read of the certification file
// (certification.SignalSourceCatalog), so a request sees one consistent view.
type certSnapshot map[string]signalscan.Certification

func (s certSnapshot) ValidSource(id string) (signalscan.Certification, bool) {
	c, ok := s[id]
	return c, ok
}

func certSnapshotFor(d Deps, now func() time.Time) certSnapshot {
	snap := certSnapshot{}
	for _, e := range certification.SignalSourceCatalog(certification.Options{
		Path: d.Flags.CertificationPath, Key: d.Flags.CertificationKey.Reveal(), BuildSHA: d.Flags.BuildSHA, Now: now,
	}) {
		if e.Certification == nil {
			continue
		}
		c := e.Certification
		snap[e.ID] = signalscan.Certification{
			ValidatedAt: certText(c.ValidatedAt), ExpiresAt: certText(c.ExpiresAt), BuildSHA: certText(c.BuildSHA),
			ValidationRunID: certText(c.ValidationRunID), EvidenceURL: certText(c.EvidenceURL),
			EvidenceSHA256: certText(c.EvidenceSHA256), Mode: certText(c.Mode),
		}
	}
	return snap
}

// certText renders a certificate field; validated certificates carry strings there.
func certText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
