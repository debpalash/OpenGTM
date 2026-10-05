//go:build unix

package process

import (
	"context"
	"strings"
	"testing"
)

func checkNamed(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", name, checks)
	return Check{}
}

func TestPreflightReportsAWorkingHost(t *testing.T) {
	requirePython(t)
	checks := Preflight(context.Background(), Options{PythonPath: []string{sdkPath()}, StateDir: t.TempDir()})
	for _, name := range []string{"python interpreter", "opengtm-sdk", "plugin state dir", "sandbox"} {
		if c := checkNamed(t, checks, name); !c.OK {
			t.Errorf("%s: %s", name, c.Detail)
		}
	}
}

func TestPreflightExplainsWhatIsMissing(t *testing.T) {
	requirePython(t)
	// The SDK is not importable from an empty path.
	checks := Preflight(context.Background(), Options{PythonPath: []string{t.TempDir()}, StateDir: t.TempDir()})
	if c := checkNamed(t, checks, "opengtm-sdk"); c.OK || !strings.Contains(c.Detail, "OPENGTM_PLUGIN_PYTHONPATH") {
		t.Errorf("sdk check: %+v", c)
	}
	// No interpreter.
	checks = Preflight(context.Background(), Options{Python: "no-such-python-binary"})
	if c := checkNamed(t, checks, "python interpreter"); c.OK || !strings.Contains(c.Detail, "OPENGTM_PLUGIN_PYTHON") {
		t.Errorf("python check: %+v", c)
	}
	// An unusable state directory (a file where the directory should be).
	checks = Preflight(context.Background(), Options{PythonPath: []string{sdkPath()}, StateDir: "/dev/null/state"})
	if c := checkNamed(t, checks, "plugin state dir"); c.OK {
		t.Errorf("state dir check: %+v", c)
	}
}

func TestPreflightProbesTheBwrapSandbox(t *testing.T) {
	requirePython(t)
	checks := Preflight(context.Background(), Options{PythonPath: []string{sdkPath()}, StateDir: t.TempDir(), Launcher: BwrapLauncher{Path: "/no/such/bwrap"}})
	if c := checkNamed(t, checks, "sandbox (bwrap)"); c.OK {
		t.Errorf("a missing bwrap must fail the check: %+v", c)
	}
}

func TestOptionsFromEnv(t *testing.T) {
	env := map[string]string{
		"OPENGTM_PLUGIN_MAX_PROCESSES": "7", "OPENGTM_PLUGIN_MAX_PER_PLUGIN": "3", "OPENGTM_PLUGIN_PYTHON": "/opt/py/bin/python",
		"OPENGTM_PLUGIN_PYTHONPATH": "/a:/b", "OPENGTM_PLUGIN_STATE_DIR": "/var/lib/x", "OPENGTM_PLUGIN_SANDBOX": "bwrap",
		"OPENGTM_PLUGIN_SANDBOX_RO": "/venv",
	}
	look := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	o, err := OptionsFromEnv(Options{}, look)
	if err != nil {
		t.Fatal(err)
	}
	if o.MaxProcesses != 7 || o.MaxPerPlugin != 3 || o.Python != "/opt/py/bin/python" || len(o.PythonPath) != 2 ||
		o.StateDir != "/var/lib/x" || o.Launcher.Name() != "bwrap" || len(o.ReadOnlyPaths) != 1 {
		t.Fatalf("options: %+v", o)
	}
	for k, bad := range map[string]string{"OPENGTM_PLUGIN_MAX_PROCESSES": "0", "OPENGTM_PLUGIN_MAX_PER_PLUGIN": "many", "OPENGTM_PLUGIN_SANDBOX": "docker"} {
		env := map[string]string{k: bad}
		if _, err := OptionsFromEnv(Options{}, func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err == nil {
			t.Errorf("%s=%q accepted", k, bad)
		}
	}
}
