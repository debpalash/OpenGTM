//go:build linux

package process

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startWorker re-executes the test binary as a stand-in worker that supervises
// a plugin tree, and returns once the tree is running.
func startWorker(t *testing.T, state string, noPdeathsig bool) (*exec.Cmd, treePIDs) {
	t.Helper()
	requirePython(t)
	pidfile := filepath.Join(t.TempDir(), "pids.json")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"=serve", "HELPER_STATE="+state, "HELPER_SDK="+sdkPath(), "HELPER_PIDFILE="+pidfile)
	if noPdeathsig {
		cmd.Env = append(cmd.Env, "HELPER_NO_PDEATHSIG=1")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if !strings.HasPrefix(line, "READY") {
			t.Fatalf("worker did not become ready: %q", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("worker never became ready")
	}
	return cmd, readTree(t, pidfile)
}

func killWorker(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}

func supDirs(t *testing.T, state string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(state, "sup-*"))
	return m
}

func TestWorkerCrashPluginDiesWithItAndSweepReapsTheRest(t *testing.T) {
	state := t.TempDir()
	cmd, pids := startWorker(t, state, false)
	pids.assertAlive(t)

	killWorker(t, cmd) // SIGKILL: no deferred cleanup runs

	// The kernel kills the plugin process itself (PR_SET_PDEATHSIG), ...
	waitGone(t, "plugin process", pids.Self, 3*time.Second)
	// ... but its background children are not direct children of the worker,
	// so they survive. This is why the sweep exists.
	time.Sleep(200 * time.Millisecond)
	if procGone(pids.SameGroup) && procGone(pids.Setsid) && procGone(pids.Orphan) {
		t.Fatal("control: background processes were expected to outlive the crashed worker")
	}
	if len(supDirs(t, state)) != 1 {
		t.Fatalf("crash left no state to recover: %v", supDirs(t, state))
	}

	// A restarted worker sweeps the dead one's leftovers.
	log := &captureLog{}
	s := newSup(t, Options{StateDir: state, Logger: log})
	pids.assertGone(t, 3*time.Second)
	for _, d := range supDirs(t, state) {
		if !strings.Contains(d, "sup-"+itoaPID(os.Getpid())+"-") {
			t.Fatalf("dead supervisor state not removed: %s", d)
		}
	}
	if !strings.Contains(log.String(), "reclaimed plugin state") {
		t.Fatalf("sweep not logged:\n%s", log.String())
	}
	// And the new supervisor is fully functional.
	mustRun(t, s, pspec{}.build(t), map[string]any{"mode": "ok"}, nil)
}

func TestSweepKillsAPluginTheKernelDidNotReap(t *testing.T) {
	state := t.TempDir()
	cmd, pids := startWorker(t, state, true) // no PR_SET_PDEATHSIG
	killWorker(t, cmd)
	time.Sleep(300 * time.Millisecond)
	pids.assertAlive(t) // control: everything outlives the worker

	res, err := Sweep(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Supervisors != 1 || res.Killed != 1 || res.Removed != 1 {
		t.Fatalf("sweep result: %+v", res)
	}
	pids.assertGone(t, 3*time.Second)
	if len(supDirs(t, state)) != 0 {
		t.Fatalf("state left behind: %v", supDirs(t, state))
	}
	// Sweeping again is a no-op.
	if res, _ := Sweep(state, nil); res.Supervisors != 0 {
		t.Fatalf("second sweep: %+v", res)
	}
}

func TestSweepLeavesLiveSupervisorsAlone(t *testing.T) {
	state := t.TempDir()
	a := newSup(t, Options{StateDir: state})
	p := pspec{timeout: 30}.build(t)
	done := make(chan error, 1)
	go func() {
		_, err := run(t, a, p, map[string]any{"mode": "hold_slot", "seconds": 1.0}, nil)
		done <- err
	}()
	for a.Stats().Running == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// A second worker (or `opengtm plugin test`) starts on the same host.
	b := newSup(t, Options{StateDir: state})
	if err := <-done; err != nil {
		t.Fatalf("a live supervisor's run was disturbed by another supervisor's start-up sweep: %v", err)
	}
	if len(supDirs(t, state)) != 2 {
		t.Fatalf("both supervisors should keep their state: %v", supDirs(t, state))
	}
	mustRun(t, b, pspec{}.build(t), map[string]any{"mode": "ok"}, nil)
}

func TestSweepNeverSignalsUnrelatedProcesses(t *testing.T) {
	state := t.TempDir()
	dead := filepath.Join(state, "sup-1999999-deadbeef")
	if err := os.Mkdir(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	// The owner is gone, and a stale record points at this very test process
	// with a start time that does not match: PID reuse after a reboot.
	writeFile := func(name string, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dead, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("owner.json", ownerFile{PID: 1999999, Start: 1})
	writeFile("r-stale.json", runFile{PID: os.Getpid(), PGID: os.Getpid(), Start: 1, Token: "no-process-carries-this-token", Plugin: "stale"})
	if err := os.Mkdir(filepath.Join(dead, "r-stale"), 0o700); err != nil {
		t.Fatal(err)
	}

	res, err := Sweep(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Supervisors != 1 || res.Killed != 0 || res.Removed != 1 {
		t.Fatalf("sweep: %+v", res)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("stale state not removed: %v", err)
	}
	// Reaching this line means the sweep did not kill the test process or its group.
}

func TestStateDirectoryMustBeOurs(t *testing.T) {
	requirePython(t)
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSupervisor(Options{StateDir: link}); err == nil {
		t.Fatal("a symlinked state directory was accepted")
	}
	// An existing directory of ours that is too open is tightened.
	s, err := NewSupervisor(Options{StateDir: real})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode %v", fi.Mode().Perm())
	}
}

func itoaPID(pid int) string {
	b, _ := json.Marshal(pid)
	return string(b)
}
