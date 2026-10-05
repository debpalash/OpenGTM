package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Check is one preflight result, shaped like `opengtm doctor` rows.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Preflight reports whether this host can run process plugins with opts: a
// supported platform, an interpreter that can import the SDK, a usable state
// directory and, when selected, a working sandbox. `opengtm doctor` runs it
// when the plugin catalog contains process plugins.
func Preflight(ctx context.Context, opts Options) []Check {
	var out []Check
	add := func(name string, ok bool, format string, a ...any) {
		out = append(out, Check{name, ok, fmt.Sprintf(format, a...)})
	}
	if !Supported {
		add("process plugins", false, "%v", ErrUnsupported)
		return out
	}

	python := opts.Python
	if python == "" {
		python = "python3"
	}
	path, err := exec.LookPath(python)
	if err != nil {
		add("python interpreter", false, "%q not found on PATH; install Python or set OPENGTM_PLUGIN_PYTHON", python)
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ver, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		add("python interpreter", false, "%s --version: %v", path, err)
		return out
	}
	add("python interpreter", true, "%s (%s)", path, strings.TrimSpace(string(ver)))

	cmd := exec.CommandContext(ctx, path, "-c", "import opengtm_sdk; print(opengtm_sdk.__version__)")
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + os.TempDir()}
	if len(opts.PythonPath) > 0 {
		cmd.Env = append(cmd.Env, "PYTHONPATH="+strings.Join(opts.PythonPath, string(os.PathListSeparator)))
	}
	if sdk, err := cmd.CombinedOutput(); err != nil {
		add("opengtm-sdk", false, "the interpreter cannot import opengtm_sdk (pip install opengtm-sdk, or set OPENGTM_PLUGIN_PYTHONPATH): %s",
			oneLine(string(sdk), 200))
	} else {
		add("opengtm-sdk", true, "version %s", strings.TrimSpace(string(sdk)))
	}

	dir := opts.StateDir
	if dir == "" {
		dir = defaultStateDir()
	}
	if err := checkStateDir(dir); err != nil {
		add("plugin state dir", false, "%v", err)
	} else {
		add("plugin state dir", true, "%s is usable", dir)
	}

	if bw, ok := opts.Launcher.(BwrapLauncher); ok {
		tmp, err := os.MkdirTemp("", "opengtm-preflight-")
		if err == nil {
			defer os.RemoveAll(tmp)
			var cmd *exec.Cmd
			cmd, err = bw.Command(LaunchSpec{
				Argv: []string{"/usr/bin/true"}, PluginDir: tmp, RunDir: tmp, Env: []string{"PATH=/usr/bin:/bin"}, ControlFile: os.Stdin,
			})
			if err == nil {
				var b []byte
				if b, err = cmd.CombinedOutput(); err != nil {
					err = fmt.Errorf("%v: %s", err, oneLine(string(b), 200))
				}
			}
		}
		if err != nil {
			add("sandbox (bwrap)", false, "%v (needs bubblewrap and unprivileged user namespaces)", err)
		} else {
			add("sandbox (bwrap)", true, "bubblewrap can create the sandbox")
		}
	} else {
		add("sandbox", true, "exec launcher: environment scrubbing and resource limits, no filesystem or network isolation")
	}
	return out
}

func checkStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".preflight-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(filepath.Clean(name))
}
