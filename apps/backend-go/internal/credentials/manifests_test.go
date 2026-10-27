package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// The worker serves verified manifests from its own connector index (plan 15 cutover: the Python
// GET /internal/v1/manifests/{name} is gone). Unknown names are ErrManifestNotFound; the sha256 is the index
// entry's, so it matches the one the API planner put in the run payload.
func TestLocalManifestsServeTheIndex(t *testing.T) {
	idx := ConnectorIndex(func(string) (string, bool) { return "", false }, nil) // built-ins, policy optional
	entries := idx()
	e, ok := entries["leadmagic_email"]
	if !ok || e.SHA256 == "" {
		t.Fatalf("built-in index lacks leadmagic_email: %d entries", len(entries))
	}
	render := func(m manifest.Manifest) ([]byte, error) { return json.Marshal(map[string]string{"name": m.Name}) }
	src := NewLocalManifests(idx, render)
	raw, sha, err := src.Manifest(context.Background(), "leadmagic_email")
	if err != nil || sha != e.SHA256 || string(raw) != `{"name":"leadmagic_email"}` {
		t.Fatalf("got %s %s %v", raw, sha, err)
	}
	if _, _, err := src.Manifest(context.Background(), "no-such-provider"); !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, _, err := src.Manifest(context.Background(), ""); err == nil {
		t.Fatal("empty name accepted")
	}
}

// A set-but-empty CONNECTOR_SIGNATURE_POLICY is "required" (as Python); unsigned built-ins then drop out.
func TestConnectorIndexPolicy(t *testing.T) {
	optional := ConnectorIndex(func(string) (string, bool) { return "", false }, nil)()
	required := ConnectorIndex(func(k string) (string, bool) {
		if k == "CONNECTOR_SIGNATURE_POLICY" {
			return "", true
		}
		return "", false
	}, nil)()
	if len(required) > len(optional) {
		t.Fatalf("required %d > optional %d", len(required), len(optional))
	}
}
