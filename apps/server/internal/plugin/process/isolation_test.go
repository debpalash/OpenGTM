//go:build linux

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hostSecrets plants the kind of values a real worker has in its environment.
func hostSecrets(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{
		"DATABASE_URL":          "postgres://opengtm:hunter2-db-password@db.internal/opengtm",
		"STRIPE_SECRET_KEY":     "sk_live_STRIPE-NEEDLE-4242",
		"AWS_SECRET_ACCESS_KEY": "AWS-NEEDLE-abcdef0123456789",
		"OPENGTM_LEGACY_TOKEN":  "legacy-NEEDLE-token-9876",
		"KEY_B":                 "kb-NEEDLE-undeclared-1111",
	}
	for k, v := range m {
		t.Setenv(k, v)
	}
	return m
}

var allowedEnv = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true,
	"PYTHONUNBUFFERED": true, "PYTHONDONTWRITEBYTECODE": true, "PYTHONHASHSEED": true, "PYTHONPATH": true,
	"MALLOC_ARENA_MAX": true, "OPENGTM_PLUGIN_FD": true, "OPENGTM_PLUGIN_PROTOCOL": true,
	"OPENGTM_PLUGIN_DIR": true, MarkerEnv: true,
	// not set by us in tests, tolerated if the host has them
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "TZ": true,
	// added by the interpreter or by the /bin/sh shim, not inherited
	"LC_CTYPE": true, "PWD": true, "OLDPWD": true, "SHLVL": true, "_": true,
}

func TestEnvironmentIsScrubbed(t *testing.T) {
	secrets := hostSecrets(t)
	t.Setenv("PATH_EXTRA_SENTINEL", "should-not-pass")
	s := newSup(t, Options{})
	out := mustRun(t, s, pspec{}.build(t), map[string]any{"mode": "env"}, nil)
	f := fieldsOf(t, out)
	env := asMap(t, f["env"])

	var extra []string
	for k := range env {
		if !allowedEnv[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Fatalf("plugin inherited environment variables outside the allow-list: %v", extra)
	}
	for k, v := range env {
		for name, secret := range secrets {
			if strings.Contains(fmt.Sprint(v), secret) {
				t.Fatalf("host secret %s leaked into plugin env var %s", name, k)
			}
		}
	}
	if env["HOME"] != f["home"] || env["TMPDIR"] != f["home"] || !strings.Contains(fmt.Sprint(f["home"]), "r-") {
		t.Fatalf("HOME/TMPDIR should be the private run directory: %v", env)
	}
}

func TestOnlyDeclaredSecretsAreDelivered(t *testing.T) {
	hostSecrets(t)
	s := newSup(t, Options{})
	req := map[string]string{"KEY_A": "ka-declared-value-1", "KEY_B": "kb-undeclared-value-2"}

	declared := pspec{secrets: []string{"KEY_A"}}.build(t)
	f := fieldsOf(t, mustRun(t, s, declared, map[string]any{"mode": "env"}, req))
	got := asMap(t, f["secret_rev"])
	if len(got) != 1 || got["KEY_A"] != "1-eulav-deralced-ak" {
		t.Fatalf("declared plugin received %v", got)
	}
	// Delivered through the control channel, never the environment.
	for k, v := range asMap(t, f["env"]) {
		if strings.Contains(fmt.Sprint(v), "ka-declared") || k == "KEY_A" {
			t.Fatalf("declared secret appeared in the environment: %s", k)
		}
	}

	undeclared := pspec{}.build(t)
	f = fieldsOf(t, mustRun(t, s, undeclared, map[string]any{"mode": "env"}, req))
	if got := asMap(t, f["secret_rev"]); len(got) != 0 {
		t.Fatalf("plugin with no declared secrets received %v", got)
	}
}

// scanFromWorker re-executes the test binary as a stand-in worker whose
// initial environment holds the planted secrets, runs a plugin that hunts for
// one of them everywhere a same-user process can look, and returns the paths
// where it was found.
func scanFromWorker(t *testing.T, keepDumpable bool, needle string, extraEnv ...string) (workerPID int, found []string) {
	t.Helper()
	requirePython(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		helperEnv+"=scan", "HELPER_STATE="+t.TempDir(), "HELPER_SDK="+sdkPath(), "HELPER_NEEDLE="+needle,
		"DATABASE_URL=postgres://opengtm:hunter2-db-password@db.internal/opengtm", "STRIPE_SECRET_KEY=sk_live_STRIPE-NEEDLE-4242")
	cmd.Env = append(cmd.Env, extraEnv...)
	if keepDumpable {
		cmd.Env = append(cmd.Env, "HELPER_KEEP_DUMPABLE=1")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("worker helper: %v %s", err, out)
	}
	var res struct {
		PID   int      `json:"pid"`
		Found []string `json:"found"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("worker helper output %q: %v", out, err)
	}
	return res.PID, res.Found
}

func TestWorkerSecretsAreNotReachableThroughProc(t *testing.T) {
	const needle = "kb-NEEDLE-undeclared-1111"
	extra := "KEY_B=" + needle

	// Positive control: with a dumpable worker, a same-user plugin CAN read the
	// secret from /proc/<worker>/environ. The scan would see the leak, so an
	// empty result below means something.
	pid, found := scanFromWorker(t, true, needle, extra)
	want := fmt.Sprintf("/proc/%d/environ", pid)
	if !slices.Contains(found, want) {
		t.Fatalf("control failed: scan did not find the secret in %s (found %v); the hardened result below would prove nothing", want, found)
	}

	// Default: the supervisor clears PR_SET_DUMPABLE, so /proc/<worker>/environ
	// is owned by root and unreadable by the plugin.
	pid, found = scanFromWorker(t, false, needle, extra)
	for _, path := range found {
		if strings.HasPrefix(path, fmt.Sprintf("/proc/%d/", pid)) {
			t.Fatalf("an undeclared worker secret is reachable by the plugin through %s", path)
		}
	}
}

func TestDeliveredSecretsLiveOnlyInThePluginProcess(t *testing.T) {
	s := newSup(t, Options{})
	declared := pspec{secrets: []string{"KEY_A"}}.build(t)
	const secret = "ka-declared-needle-77"
	f := fieldsOf(t, mustRun(t, s, declared, map[string]any{"mode": "scan_fs", "needle": secret}, map[string]string{"KEY_A": secret}))
	if got := f["found"].([]any); len(got) != 0 {
		t.Fatalf("a delivered secret is visible outside the plugin's memory (env, argv, files): %v", got)
	}
	// And it was actually delivered (the scan is not vacuous).
	e := fieldsOf(t, mustRun(t, s, declared, map[string]any{"mode": "env"}, map[string]string{"KEY_A": secret}))
	if len(asMap(t, e["secret_rev"])) != 1 {
		t.Fatal("secret was not delivered")
	}
}

func TestRunDirectoryIsPrivateEmptyAndRemoved(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{}.build(t)
	f := fieldsOf(t, mustRun(t, s, p, map[string]any{"mode": "env"}, nil))
	if f["tmpdir_mode"] != "0o700" {
		t.Fatalf("run directory mode %v", f["tmpdir_mode"])
	}
	if l := f["home_listing"].([]any); len(l) != 0 {
		t.Fatalf("run directory not empty at start: %v", l)
	}
	if f["cwd"] != testdataDir() {
		t.Fatalf("cwd %v, want the plugin directory %s", f["cwd"], testdataDir())
	}

	// State written by one run never reaches the next.
	mustRun(t, s, p, map[string]any{"mode": "write_files", "content": "tenant-a-data"}, nil)
	f = fieldsOf(t, mustRun(t, s, p, map[string]any{"mode": "env"}, nil))
	if l := f["home_listing"].([]any); len(l) != 0 {
		t.Fatalf("a previous run's files are visible: %v", l)
	}
	left, _ := filepath.Glob(filepath.Join(s.supDir, "r-*"))
	if len(left) != 0 {
		t.Fatalf("run directories not removed: %v", left)
	}
}

func TestOnlyTheControlSocketIsInherited(t *testing.T) {
	// An open file in the worker must not leak into plugins.
	f, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := newSup(t, Options{})
	out := fieldsOf(t, mustRun(t, s, pspec{}.build(t), map[string]any{"mode": "env"}, nil))
	for _, v := range out["fds"].([]any) {
		if fd := int(v.(float64)); fd > 4 { // 0-2 stdio, 3 control, 4 is the directory listing itself
			t.Fatalf("plugin inherited descriptor %d: %v", fd, out["fds"])
		}
	}
	if out["uid"] != float64(os.Getuid()) {
		t.Fatalf("uid %v", out["uid"])
	}
}

func TestResourceLimitsAreApplied(t *testing.T) {
	s := newSup(t, Options{NoFile: 64})
	p := pspec{timeout: 10, memory: 100}.build(t)
	f := fieldsOf(t, mustRun(t, s, p, map[string]any{"mode": "limits"}, nil))
	pair := func(k string) (float64, float64) {
		l := f[k].([]any)
		return l[0].(float64), l[1].(float64)
	}
	if soft, hard := pair("nofile"); soft != 64 || hard != 64 {
		t.Fatalf("RLIMIT_NOFILE = %v/%v", soft, hard)
	}
	wantAS := float64((100 + memoryHeadroomMB) << 20)
	if soft, hard := pair("address_space"); soft != wantAS || hard != wantAS {
		t.Fatalf("RLIMIT_AS = %v/%v, want %v", soft, hard, wantAS)
	}
	if soft, hard := pair("cpu"); soft != 16 || hard != 16 {
		t.Fatalf("RLIMIT_CPU = %v/%v, want timeout+6", soft, hard)
	}
	if soft, hard := pair("core"); soft != 0 || hard != 0 {
		t.Fatalf("RLIMIT_CORE = %v/%v", soft, hard)
	}
}

func TestFileDescriptorLimitIsEnforced(t *testing.T) {
	s := newSup(t, Options{NoFile: 64})
	f := fieldsOf(t, mustRun(t, s, pspec{}.build(t), map[string]any{"mode": "fd_bomb"}, nil))
	if opened := f["opened"].(float64); opened > 64 || f["errno"] != float64(syscall.EMFILE) {
		t.Fatalf("plugin opened %v descriptors (errno %v) under a limit of 64", f["opened"], f["errno"])
	}
}

func TestMemoryLimitIsEnforced(t *testing.T) {
	s := newSup(t, Options{})
	// 2 GiB cannot be allocated under memory_mb: 64.
	_, err := run(t, s, pspec{memory: 64}.build(t), map[string]any{"mode": "oom"}, nil)
	pe := wantError(t, err, CodePluginException, false)
	if !strings.Contains(pe.Message, "MemoryError") {
		t.Fatalf("message: %s", pe.Message)
	}
	// The same allocation is possible with a large enough limit.
	out := fieldsOf(t, mustRun(t, newSup(t, Options{}), pspec{memory: 4096}.build(t), map[string]any{"mode": "oom"}, nil))
	if out["allocated"] != float64(2<<30) {
		t.Fatalf("control: %v", out)
	}
}

func TestSecretsNeverAppearInOutputsErrorsOrLogs(t *testing.T) {
	log := &captureLog{}
	s := newSup(t, Options{Logger: log})
	const secret = "sk-very-secret-value-0001"
	p := pspec{secrets: []string{"API_KEY"}}.build(t)
	req := map[string]string{"API_KEY": secret}

	var prog string
	out, err := s.Run(t.Context(), p, Request{Inputs: map[string]any{"mode": "leak"}, Secrets: req,
		OnProgress: func(pr Progress) { prog += pr.Message }})
	if err != nil {
		t.Fatal(err)
	}
	blob := fmt.Sprint(out.Records, out.ProviderError, out.Stopped, prog)
	if strings.Contains(blob, secret) || !strings.Contains(blob, "REDACTED") {
		t.Fatalf("secret in the outcome: %s", blob)
	}
	if strings.Contains(log.String(), secret) {
		t.Fatalf("secret in logs:\n%s", log.String())
	}

	_, err = run(t, s, p, map[string]any{"mode": "fail", "code": "upstream_error", "message": "denied for key " + secret}, req)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("error: %v", err)
	}
	_, err = run(t, s, p, map[string]any{"mode": "exit1", "leak": secret}, req)
	if pe, ok := err.(*Error); !ok || strings.Contains(pe.Message+pe.Stderr, secret) {
		t.Fatalf("crash diagnostics leak the secret: %v", err)
	}
}

// bwrapUsable reports whether the bubblewrap launcher can start a process
// here (it needs unprivileged user namespaces).
func bwrapUsable(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		return false
	}
	dir := t.TempDir()
	cmd, err := BwrapLauncher{}.Command(LaunchSpec{
		Argv: []string{"/usr/bin/true"}, PluginDir: dir, RunDir: dir,
		Env: []string{"PATH=/usr/bin:/bin"}, ControlFile: os.Stdin,
	})
	if err != nil {
		return false
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("bwrap probe failed: %v: %s", err, out)
		return false
	}
	return true
}

func TestBwrapLauncherAddsFilesystemAndNetworkIsolation(t *testing.T) {
	if !bwrapUsable(t) {
		t.Skip("bubblewrap with user namespaces is not available")
	}
	secretFile := filepath.Join(os.TempDir(), fmt.Sprintf("opengtm-bwrap-test-%d", os.Getpid()))
	if err := os.WriteFile(secretFile, []byte("host-file-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secretFile)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	p := pspec{}.build(t)

	// Control: the default launcher cannot stop a plugin from reading a host
	// file or opening its own socket. This is the documented limitation.
	plain := newSup(t, Options{})
	f := fieldsOf(t, mustRun(t, plain, p, map[string]any{"mode": "readfile", "path": secretFile}, nil))
	if f["readable"] != true {
		t.Fatalf("control: %v", f)
	}
	f = fieldsOf(t, mustRun(t, plain, p, map[string]any{"mode": "connect", "host": "127.0.0.1", "port": port}, nil))
	if f["connected"] != true {
		t.Fatalf("control: %v", f)
	}

	sandboxed := newSup(t, Options{Launcher: BwrapLauncher{}})
	f = fieldsOf(t, mustRun(t, sandboxed, p, map[string]any{"mode": "readfile", "path": secretFile}, nil))
	if f["readable"] != false {
		t.Fatalf("sandboxed plugin read a host file: %v", f)
	}
	f = fieldsOf(t, mustRun(t, sandboxed, p, map[string]any{"mode": "connect", "host": "127.0.0.1", "port": port}, nil))
	if f["connected"] != false {
		t.Fatalf("sandboxed plugin opened a network connection: %v", f)
	}
	// The worker's process is not even visible inside the PID namespace.
	f = fieldsOf(t, mustRun(t, sandboxed, p, map[string]any{"mode": "readfile", "path": fmt.Sprintf("/proc/%d/environ", os.Getpid())}, nil))
	if f["readable"] != false || f["error"] != "FileNotFoundError" {
		t.Fatalf("sandboxed plugin can see the worker process: %v", f)
	}
	// The sandbox still honours the protocol, limits and kill semantics.
	pidfile := "/tmp/pids.json" // /tmp is a private tmpfs inside the sandbox
	tp := pspec{timeout: 1.5}.build(t)
	_, err = run(t, sandboxed, tp, map[string]any{"mode": "tree", "pidfile": pidfile}, nil)
	wantError(t, err, CodeTimeout, true)
}

// A plugin running at the same time as another one, as the same OS user, can
// read the other's /proc/<pid>/environ unless the other hardened itself. The
// SDK clears PR_SET_DUMPABLE at start-up, so SDK plugins (where declared
// secrets live, in memory, after the init frame) are not readable; a plugin
// that does not use the SDK is, which is why the sandbox launcher exists.
func TestConcurrentPluginsCannotReadEachOthersProcEntries(t *testing.T) {
	seen := peekAtConcurrentPlugins(t, false)
	sdkSeen, rawSeen := fmt.Sprint(seen["sdk_all.py"]), fmt.Sprint(seen["rawplugin.py"])
	if !strings.Contains(sdkSeen, "PermissionError") || strings.Contains(sdkSeen, "readable") {
		t.Fatalf("an SDK plugin's /proc entries are readable by another plugin: %v", seen)
	}
	// Control: a hand-written plugin that does not harden itself is readable,
	// so the check above would have noticed.
	if !strings.Contains(rawSeen, "readable") {
		t.Fatalf("control failed, the attacker could not read an unhardened plugin either: %v", seen)
	}
	// Control 2: the opt-out for debugging really does turn the hardening off.
	seen = peekAtConcurrentPlugins(t, true)
	if !strings.Contains(fmt.Sprint(seen["sdk_all.py"]), "readable") {
		t.Fatalf("OPENGTM_SDK_DUMPABLE=1 did not disable the SDK's hardening: %v", seen)
	}
}

// peekAtConcurrentPlugins runs an SDK plugin and a raw plugin side by side and
// reports what a third plugin sees when it tries to read their environ.
func peekAtConcurrentPlugins(t *testing.T, sdkDumpable bool) map[string]any {
	t.Helper()
	opts := Options{MaxProcesses: 4, MaxPerPlugin: 2, Client: testClient(t, okTransport())}
	if sdkDumpable {
		opts.PassEnv = []string{"OPENGTM_SDK_DUMPABLE"}
		t.Setenv("OPENGTM_SDK_DUMPABLE", "1")
	}
	s := newSup(t, opts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &captureLog{}
	s.log = log

	sdkVictim := sdkSpec("provider").build(t)
	rawVictim := pspec{name: "raw_victim", timeout: 60}.build(t)
	go func() {
		_, _ = s.Run(ctx, sdkVictim, Request{Inputs: map[string]any{"domain": "x", "mode": "sleep"}, Secrets: map[string]string{"API_KEY": "victim-key-1"}})
	}()
	go func() {
		_, _ = s.Run(ctx, rawVictim, Request{Inputs: map[string]any{"mode": "sleep", "seconds": 60}})
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(log.String(), "ready") || s.Stats().Running < 2 {
		if time.Now().After(deadline) {
			t.Fatal("victims did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	attacker := pspec{name: "attacker_plugin"}.build(t)
	f := fieldsOf(t, mustRun(t, s, attacker, map[string]any{"mode": "peek", "needles": []string{"sdk_all.py", "rawplugin.py"}}, nil))
	return asMap(t, f["seen"])
}
