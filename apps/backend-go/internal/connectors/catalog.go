package connectors

import (
	"context"
	"errors"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/certification"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// SecretGetter is secrets.Resolver.Get: workspace secret, then the global
// setting, then the process environment (Python get_secret).
type SecretGetter interface {
	Get(ctx context.Context, ws, key string) (value, source string, err error)
}

// Deps is what the catalog needs. Cert.Key is the certification HMAC key,
// passed by the caller as cfg.Flags.CertificationKey.Reveal() at the call site.
type Deps struct {
	Dir     string // manifests directory
	Policy  string // already resolved with EffectivePolicy
	Trust   manifest.TrustStore
	Secrets SecretGetter
	Cert    certification.Options
}

// ErrUnknownConnectorSubject is returned when a loadable manifest has a name
// that is not a certifiable subject (an invalid connector id). Python answers
// 500 (`certification subject is unknown`); Go answers the 4xx
// UnknownSubjectStatus (plan D4, documented in testdata/contract/README.md).
var ErrUnknownConnectorSubject = errors.New("connector manifest name is not a valid connector id")

// UnknownSubjectStatus is the HTTP status the route answers for
// ErrUnknownConnectorSubject.
const UnknownSubjectStatus = 422

// CatalogEntry is one /catalog connector in Python key order. credential_key
// is never part of it.
type CatalogEntry struct {
	ManifestVersion   string                       `json:"manifest_version"`
	ID                string                       `json:"id"`
	Name              string                       `json:"name"`
	Author            string                       `json:"author"`
	Homepage          string                       `json:"homepage"`
	License           string                       `json:"license"`
	Description       string                       `json:"description"`
	Capabilities      []string                     `json:"capabilities"`
	Tags              []string                     `json:"tags"`
	CostPerLookup     float64                      `json:"cost_per_lookup"`
	DefaultConfidence float64                      `json:"default_confidence"`
	Configured        bool                         `json:"configured"`
	AuthType          string                       `json:"auth_type"`
	Signature         any                          `json:"signature"` // Signature or UnknownSignature
	PackageSHA256     *string                      `json:"package_sha256"`
	RequiredChecks    []string                     `json:"required_checks"`
	Maturity          string                       `json:"maturity"`
	Certification     *certification.Certification `json:"certification"`
}

// CatalogResponse is GET /api/connectors/catalog.
type CatalogResponse struct {
	ManifestVersion string         `json:"manifest_version"`
	SignaturePolicy string         `json:"signature_policy"`
	Total           int            `json:"total"`
	Connectors      []CatalogEntry `json:"connectors"`
}

// Catalog builds the catalog for workspace ws from one pass over the manifest
// directory (the report and the loaded manifests come from the same bytes).
// package_sha256 is manifest.Entry.SHA256 of the reviewed manifest and feeds the
// certification build map unchanged.
func Catalog(ctx context.Context, d Deps, ws string) (CatalogResponse, error) {
	scan := scanDirectory(d.Dir, d.Policy, d.Trust)
	reviewed := map[string]Entry{}
	for _, e := range scan.report.Connectors {
		reviewed[e.ID] = e
	}
	subjects := make([]string, 0, len(scan.loaded))
	builds := map[string]string{}
	for _, l := range scan.loaded {
		id := "connector:" + l.Entry.Manifest.Name
		if !certification.Known(id) {
			return CatalogResponse{}, ErrUnknownConnectorSubject
		}
		subjects = append(subjects, id)
	}
	for name, e := range reviewed {
		builds["connector:"+name] = e.ManifestSHA256
	}
	maturity := certification.StatusFor(subjects, builds, d.Cert)
	out := CatalogResponse{ManifestVersion: "1", SignaturePolicy: scan.report.SignaturePolicy, Connectors: []CatalogEntry{}}
	for _, l := range scan.loaded {
		m := l.Entry.Manifest
		item := newEntry("", l.Entry, l.Sig)
		configured := m.Auth.Type == "none"
		if !configured && m.Auth.EnvVar != "" && !manifest.EnvDenied(m.Auth.EnvVar) {
			v, _, err := d.Secrets.Get(ctx, ws, m.Auth.EnvVar)
			if err != nil {
				return CatalogResponse{}, fmt.Errorf("connectors: secret lookup: %w", err)
			}
			configured = v != ""
		}
		ce := CatalogEntry{
			ManifestVersion: item.ManifestVersion, ID: item.ID, Name: item.Name, Author: item.Author,
			Homepage: item.Homepage, License: item.License, Description: item.Description,
			Capabilities: item.Capabilities, Tags: nonNil(item.Tags), CostPerLookup: item.CostPerLookup,
			DefaultConfidence: item.DefaultConfidence, Configured: configured, AuthType: m.Auth.Type,
			Signature: UnknownSignature{Status: "unknown"},
		}
		if r, ok := reviewed[m.Name]; ok {
			ce.Signature = r.Signature
			sha := r.ManifestSHA256
			ce.PackageSHA256 = &sha
		}
		st := maturity["connector:"+m.Name]
		ce.RequiredChecks, ce.Maturity, ce.Certification = nonNil(st.RequiredChecks), st.Maturity, st.Certification
		out.Connectors = append(out.Connectors, ce)
	}
	out.Total = len(out.Connectors)
	return out, nil
}
