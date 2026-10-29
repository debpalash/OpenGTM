package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest/builtin"
)

// ErrManifestNotFound is a definitive "no such verified manifest".
var ErrManifestNotFound = errors.New("credentials: manifest not found")

// Connector settings, shared with the API's connectors routes and run planner: the manifest directory and trust
// store override the built-ins embedded in the binary; the signature policy defaults to "optional" when unset (a
// set-but-empty value is "required", as Python).
const (
	ConnectorManifestsEnv  = "OPENGTM_CONNECTOR_MANIFESTS_DIR"
	ConnectorTrustStoreEnv = "OPENGTM_CONNECTOR_TRUST_STORE"
	ConnectorPolicyEnv     = "CONNECTOR_SIGNATURE_POLICY"
)

// ConnectorIndex returns a loader of the signature-verified manifest index (manifest.LoadIndex) over the connector
// settings lookup reads, loaded once on first use like the API's planner index. A load failure is logged and
// yields an empty index (every manifest then reads as not found). log may be nil.
func ConnectorIndex(lookup func(string) (string, bool), log *slog.Logger) func() map[string]manifest.Entry {
	if log == nil {
		log = slog.Default()
	}
	var once sync.Once
	var idx map[string]manifest.Entry
	return func() map[string]manifest.Entry {
		once.Do(func() {
			dir, _ := lookup(ConnectorManifestsEnv)
			trust, _ := lookup(ConnectorTrustStoreEnv)
			if dir == "" || trust == "" {
				bd, bt, err := builtin.Paths()
				if err != nil {
					log.Error("connector built-ins unavailable", "err", err)
					bd, bt = "builtin-unavailable/manifests", "builtin-unavailable/trusted-publishers.json"
				}
				if dir == "" {
					dir = bd
				}
				if trust == "" {
					trust = bt
				}
			}
			policy, set := lookup(ConnectorPolicyEnv)
			if !set {
				policy = "optional"
			}
			var err error
			if idx, err = manifest.LoadIndex(dir, policy, manifest.LoadTrustStore(trust)); err != nil {
				log.Warn("connector manifest index unavailable", "err", err)
				idx = map[string]manifest.Entry{}
			}
		})
		return idx
	}
}

// LocalManifests serves the signature-verified manifest of a provider and its sha256 from the connector index, in
// process (the former Python GET /internal/v1/manifests/{name}). render produces the manifest JSON the run
// payload carries (runplan.ManifestJSON), so the executor decodes the same document it was planned with.
type LocalManifests struct {
	index  func() map[string]manifest.Entry
	render func(manifest.Manifest) ([]byte, error)
}

// NewLocalManifests builds a LocalManifests over index and render.
func NewLocalManifests(index func() map[string]manifest.Entry, render func(manifest.Manifest) ([]byte, error)) *LocalManifests {
	return &LocalManifests{index: index, render: render}
}

// Manifest satisfies workbook.ManifestSource.
func (l *LocalManifests) Manifest(_ context.Context, name string) (json.RawMessage, string, error) {
	if name == "" {
		return nil, "", errors.New("credentials: manifest name is required")
	}
	e, ok := l.index()[name]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", ErrManifestNotFound, name)
	}
	raw, err := l.render(e.Manifest)
	if err != nil {
		return nil, "", fmt.Errorf("credentials: render manifest %s: %w", name, err)
	}
	return raw, e.SHA256, nil
}
