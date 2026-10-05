package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Refresh outcomes.
const (
	RefreshUnchanged  = "unchanged"
	RefreshUpdated    = "updated"
	RefreshCustomized = "kept-customized"
)

// RefreshResult describes what RefreshCompose did.
type RefreshResult struct {
	Action string
	// Previous is a copy of the replaced compose.yml (RefreshUpdated).
	Previous string
	// Proposed is where the new version was written instead of replacing a
	// customized file (RefreshCustomized).
	Proposed string
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// RefreshCompose brings compose.yml up to the version embedded in this binary,
// which is what new releases rely on (new services, changed dependencies).
// A compose.yml that opengtm wrote and nobody edited is replaced, after a copy
// is kept for rollback. One that has been edited is never touched: the new
// version is written next to it as compose.yml.new for the operator to merge.
// Supporting files the new compose references (searxng, otel) are created if
// missing and never overwritten.
func (i *Install) RefreshCompose(id string) (RefreshResult, error) {
	want, err := Asset("compose.yml")
	if err != nil {
		return RefreshResult{}, err
	}
	cur, err := os.ReadFile(i.ComposePath())
	if err != nil {
		return RefreshResult{}, err
	}
	if i.State.Profile == Full {
		for _, n := range []string{"searxng-settings.yml", "otel-collector.yaml"} {
			p := filepath.Join(i.Dir, n)
			if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
				b, err := Asset(n)
				if err != nil {
					return RefreshResult{}, err
				}
				if err := writeFileAtomic(p, b, 0o644); err != nil {
					return RefreshResult{}, err
				}
			}
		}
	}
	if bytes.Equal(cur, want) {
		i.State.ComposeSHA256 = sum(want)
		return RefreshResult{Action: RefreshUnchanged}, nil
	}
	if i.State.ComposeSHA256 == "" || sum(cur) != i.State.ComposeSHA256 {
		proposed := i.ComposePath() + ".new"
		if err := writeFileAtomic(proposed, want, 0o644); err != nil {
			return RefreshResult{}, err
		}
		return RefreshResult{Action: RefreshCustomized, Proposed: proposed}, nil
	}
	prev := filepath.Join(i.Dir, StateDir, "compose."+id+".yml")
	if err := writeFileAtomic(prev, cur, 0o644); err != nil {
		return RefreshResult{}, err
	}
	if err := writeFileAtomic(i.ComposePath(), want, 0o644); err != nil {
		return RefreshResult{}, err
	}
	i.State.ComposeSHA256 = sum(want)
	return RefreshResult{Action: RefreshUpdated, Previous: prev}, nil
}

// RestoreCompose puts back a compose.yml saved by RefreshCompose.
func (i *Install) RestoreCompose(prev string) error {
	b, err := os.ReadFile(prev)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(i.ComposePath(), b, 0o644); err != nil {
		return err
	}
	i.State.ComposeSHA256 = sum(b)
	return nil
}
