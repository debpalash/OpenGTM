package workbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// verifyManifests replaces every payload manifest with the copy served by the
// Python API, after checking its sha256 against the one the enqueuer put in the
// payload. The payload's own manifest bodies are never executed: a tampered
// payload (e.g. a redirected URL) is refused as manifest_mismatch.
func verifyManifests(ctx context.Context, src ManifestSource, p *runPayload) error {
	names := make([]string, 0, len(p.Manifests))
	for n := range p.Manifests {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 && src == nil {
		return refuse("manifest_source_unavailable")
	}
	for _, name := range names {
		raw, sha, err := src.Manifest(ctx, name)
		if errors.Is(err, credentials.ErrManifestNotFound) {
			return refuse("manifest_mismatch: %q is not a verified manifest", name)
		}
		if err != nil {
			return fmt.Errorf("workbook: fetch manifest %q: %w", name, err) // transient
		}
		if sha != p.ManifestSHA256[name] {
			return refuse("manifest_mismatch: %q hash differs from the payload", name)
		}
		var m manifest.Manifest
		if err := json.Unmarshal(raw, &m); err != nil || m.Name != name {
			return refuse("manifest_mismatch: %q is malformed", name)
		}
		p.Manifests[name] = m
	}
	return nil
}
