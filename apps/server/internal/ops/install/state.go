package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StateDir holds opengtm's own bookkeeping inside an install directory.
const StateDir = ".opengtm"

// Upgrade statuses.
const (
	StatusInProgress = "in_progress"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled_back"
)

// State records what is installed and the history of upgrades. The upgrade
// journal is written before anything changes, so `opengtm rollback` works even
// after a crash in the middle of an upgrade.
type State struct {
	Schema      int       `json:"schema"`
	Profile     Profile   `json:"profile"`
	Version     string    `json:"version"`
	AppImage    string    `json:"app_image"`
	ServerImage string    `json:"server_image"`
	InstalledAt time.Time `json:"installed_at"`
	// ComposeSHA256 is the hash of compose.yml as opengtm last wrote it; a
	// different hash on disk means the operator customized the file.
	ComposeSHA256 string    `json:"compose_sha256,omitempty"`
	History       []Upgrade `json:"history,omitempty"`
}

// Upgrade is one upgrade attempt.
type Upgrade struct {
	ID         string    `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`

	FromVersion     string `json:"from_version"`
	ToVersion       string `json:"to_version"`
	FromAppImage    string `json:"from_app_image"`
	FromServerImage string `json:"from_server_image"`
	ToAppImage      string `json:"to_app_image"`
	ToServerImage   string `json:"to_server_image"`

	// Backup is the pre-upgrade backup directory; empty if skipped.
	Backup string `json:"backup,omitempty"`
	// MigrationStarted is set just before the migration runs. Rollback only
	// needs to restore the database when this is true.
	MigrationStarted bool `json:"migration_started"`
	// ComposeBackup is the compose.yml replaced by this upgrade, if any.
	ComposeBackup string   `json:"compose_backup,omitempty"`
	FromRevisions []string `json:"from_revisions,omitempty"`
	ToRevisions   []string `json:"to_revisions,omitempty"`
}

// Install is an install directory loaded from disk.
type Install struct {
	Dir   string
	Env   *EnvFile
	State *State
}

// Paths inside the install directory.
func (i *Install) EnvPath() string     { return filepath.Join(i.Dir, ".env") }
func (i *Install) StatePath() string   { return filepath.Join(i.Dir, StateDir, "state.json") }
func (i *Install) LockPath() string    { return filepath.Join(i.Dir, StateDir, "lock") }
func (i *Install) ComposePath() string { return filepath.Join(i.Dir, "compose.yml") }

// BackupDir is where backups go by default.
func (i *Install) BackupDir() string { return filepath.Join(i.Dir, "backups") }

// DataDir is the bind-mounted data directory.
func (i *Install) DataDir() string {
	d := i.Env.Value("OPENGTM_DATA_DIR")
	if d == "" {
		d = "data"
	}
	if filepath.IsAbs(d) {
		return d
	}
	return filepath.Join(i.Dir, d)
}

// ConfigFiles lists the files a backup should carry so a restore on another
// host can recreate the install (relative to Dir).
func (i *Install) ConfigFiles() []string {
	return []string{".env", "opengtm.yaml", "compose.yml", "searxng-settings.yml", "otel-collector.yaml"}
}

// ProjectName is the Compose project name from .env.
func (i *Install) ProjectName() string { return i.Env.Value("COMPOSE_PROJECT_NAME") }

// ErrNotInstalled means the directory has no opengtm install.
var ErrNotInstalled = errors.New("not an opengtm install directory (run `opengtm init` first)")

// Load reads the install in dir.
func Load(dir string) (*Install, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	i := &Install{Dir: abs}
	if i.Env, err = ReadEnv(i.EnvPath()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", abs, ErrNotInstalled)
		}
		return nil, err
	}
	if _, err := os.Stat(i.ComposePath()); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, ErrNotInstalled)
	}
	i.State = &State{Schema: 1}
	raw, err := os.ReadFile(i.StatePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, i.State); err != nil {
			return nil, fmt.Errorf("parse %s: %w", i.StatePath(), err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// An install made by hand or by an older opengtm: derive what we can.
		i.State.Profile = Profile(i.Env.Value("OPENGTM_PROFILE"))
		i.State.Version = i.Env.Value("OPENGTM_VERSION")
		i.State.AppImage = i.Env.Value("OPENGTM_APP_IMAGE")
		i.State.ServerImage = i.Env.Value("OPENGTM_SERVER_IMAGE")
	default:
		return nil, err
	}
	return i, nil
}

// SaveState writes state.json atomically.
func (i *Install) SaveState() error {
	if err := os.MkdirAll(filepath.Dir(i.StatePath()), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(i.State, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(i.StatePath(), append(raw, '\n'), 0o600)
}

// LastUpgrade returns the most recent upgrade entry, or nil.
func (i *Install) LastUpgrade() *Upgrade {
	if n := len(i.State.History); n > 0 {
		return &i.State.History[n-1]
	}
	return nil
}
