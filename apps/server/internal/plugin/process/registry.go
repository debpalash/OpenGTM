//go:build unix

package process

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MarkerEnv is set in every plugin's environment to a per-run random token.
// The supervisor uses it to find processes that escaped their process group.
const MarkerEnv = "OPENGTM_PLUGIN_RUN"

// The state directory is how a restarted worker cleans up after a crashed
// one. Layout:
//
//	<state>/sup-<pid>-<rand>/owner.json     {"pid":..., "start":...}  (this supervisor)
//	<state>/sup-<pid>-<rand>/<run>.json     {"pid":..., "start":..., "token":...}
//	<state>/sup-<pid>-<rand>/<run>/         the run's private HOME/TMPDIR
//
// A supervisor removes its own entries as runs finish. Sweep removes the
// leftovers of supervisors whose process no longer exists: it kills every
// process that still carries a leftover run's marker and deletes the
// directories. This is the process-side half of crash reconciliation; the
// database-side half is the queue's lease recovery, which re-queues the
// attempt, and the lease-guarded commit, which makes a late result harmless.

type ownerFile struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

type runFile struct {
	PID    int    `json:"pid"`
	PGID   int    `json:"pgid"`
	Start  uint64 `json:"start"`
	Token  string `json:"token"`
	Plugin string `json:"plugin"`
}

// prepareStateDir creates the state directory (0700) and refuses one that is
// not ours: a /tmp subdirectory pre-created by another user (or a symlink to
// one) would let them swap run directories.
func prepareStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("plugin state directory %s must be a real directory owned by uid %d", dir, os.Getuid())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700) // ours but too open: close it rather than refuse
	}
	return nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ownerAlive reports whether the supervisor recorded in o is still running.
func ownerAlive(o ownerFile) bool {
	if !pidAlive(o.PID) {
		return false
	}
	if hasProcScan && o.Start != 0 {
		return procMatches(o.PID, o.Start)
	}
	return true
}

// SweepResult reports what Sweep cleaned up.
type SweepResult struct {
	Supervisors int // dead supervisors found
	Killed      int // leftover plugin processes killed
	Removed     int // run directories removed
}

// Sweep reclaims the leftovers of dead supervisors in stateDir. It is safe to
// run concurrently with live supervisors: only directories whose owner
// process is gone are touched.
func Sweep(stateDir string, log Logger) (SweepResult, error) {
	var res SweepResult
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	ents, err := os.ReadDir(stateDir)
	if os.IsNotExist(err) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "sup-") {
			continue
		}
		dir := filepath.Join(stateDir, e.Name())
		var owner ownerFile
		if b, err := os.ReadFile(filepath.Join(dir, "owner.json")); err == nil {
			_ = json.Unmarshal(b, &owner)
		}
		if owner.PID != 0 && ownerAlive(owner) {
			continue
		}
		res.Supervisors++
		runs, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, f := range runs {
			if filepath.Base(f) == "owner.json" {
				continue
			}
			var rf runFile
			if b, err := os.ReadFile(f); err != nil || json.Unmarshal(b, &rf) != nil || rf.Token == "" {
				continue
			}
			// The marker token is unforgeable and unique, so killing by it
			// cannot hit an unrelated process even if PIDs were reused.
			// The group is signalled only while the recorded leader is the
			// same process (same start time).
			sameLeader := hasProcScan && rf.Start != 0 && procMatches(rf.PID, rf.Start)
			if sameLeader || (hasProcScan && len(treeMembers(0, rf.Token)) > 0) {
				pgid := 0 // never signal a group whose leader we cannot identify
				if sameLeader {
					pgid = rf.PGID
				}
				killTree(pgid, rf.Token, !sameLeader)
				res.Killed++
				if log != nil {
					log.Warn("killed plugin process left by a dead supervisor", "plugin", rf.Plugin, "pid", rf.PID)
				}
			}
		}
		runDirs, _ := os.ReadDir(dir)
		for _, d := range runDirs {
			if d.IsDir() {
				res.Removed++
			}
		}
		_ = os.RemoveAll(dir)
	}
	return res, nil
}
