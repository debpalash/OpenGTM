package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// ResourceLimits are applied to a plugin process by the launcher, where the
// operating system allows. They are best effort by design: a limit the OS
// refuses (for example RLIMIT_AS on macOS) is skipped rather than failing the
// run, and the wall-clock timeout plus process-group kill remain the backstop.
type ResourceLimits struct {
	// AddressSpaceBytes is RLIMIT_AS: allocations beyond it fail (Python
	// raises MemoryError). 0 means unlimited.
	AddressSpaceBytes uint64
	// CPUSeconds is RLIMIT_CPU: the kernel kills the process after this much
	// CPU time. 0 means unlimited.
	CPUSeconds int
	// NoFile is RLIMIT_NOFILE.
	NoFile int
}

// LaunchSpec describes one plugin process to start.
type LaunchSpec struct {
	// Argv is the resolved command; Argv[0] is an absolute path.
	Argv []string
	// PluginDir is the plugin's installed directory (the working directory).
	PluginDir string
	// RunDir is a private, empty, 0700 directory for this run. HOME and
	// TMPDIR point at it and it is deleted afterwards.
	RunDir string
	// Env is the complete environment (already scrubbed).
	Env []string
	// ControlFile becomes file descriptor 3 in the child.
	ControlFile *os.File
	Limits      ResourceLimits
	// ReadOnlyPaths are host paths a sandbox must expose read-only.
	ReadOnlyPaths []string
}

// Launcher builds the command that starts a plugin process. The supervisor
// adds process-group, parent-death and I/O settings and starts it.
type Launcher interface {
	Name() string
	Command(spec LaunchSpec) (*exec.Cmd, error)
}

// shimScript applies resource limits and then replaces itself with the
// plugin command, so the plugin keeps the shell's PID and process group.
// Values are formatted from integers; argv travels as positional parameters,
// never inside the script text.
func shimScript(l ResourceLimits) string {
	s := "ulimit -c 0 2>/dev/null\n"
	if l.NoFile > 0 {
		s += "ulimit -n " + strconv.Itoa(l.NoFile) + " 2>/dev/null\n"
	}
	if l.CPUSeconds > 0 {
		s += "ulimit -t " + strconv.Itoa(l.CPUSeconds) + " 2>/dev/null\n"
	}
	if l.AddressSpaceBytes > 0 {
		s += "ulimit -v " + strconv.FormatUint((l.AddressSpaceBytes+1023)/1024, 10) + " 2>/dev/null\n"
	}
	return s + "exec \"$@\"\n"
}

// ExecLauncher is the default, dependency-free launcher: a plain child
// process in its own process group with a scrubbed environment, a private
// HOME/TMPDIR, resource limits and no inherited file descriptors but the
// control socket. It is NOT a security boundary against hostile code running
// as the same OS user: it cannot hide the filesystem or the network. Use
// BwrapLauncher (or a container) when plugins are untrusted.
type ExecLauncher struct{}

// Name implements Launcher.
func (ExecLauncher) Name() string { return "exec" }

// Command implements Launcher.
func (ExecLauncher) Command(spec LaunchSpec) (*exec.Cmd, error) {
	args := append([]string{"-c", shimScript(spec.Limits), "opengtm-plugin"}, spec.Argv...)
	cmd := exec.Command("/bin/sh", args...)
	cmd.Dir = spec.PluginDir
	cmd.Env = spec.Env
	cmd.ExtraFiles = []*os.File{spec.ControlFile}
	return cmd, nil
}

// BwrapLauncher runs the plugin inside bubblewrap (https://github.com/containers/bubblewrap):
// a new mount namespace with only the system directories and the plugin
// directory read-only, a private /tmp and /proc, no network (the only way out
// is the host fetch API over the control socket), a new PID namespace and
// IPC/UTS namespaces, and death with the supervisor. It needs unprivileged
// user namespaces. Plugins that open their own sockets (browser automation,
// vendor SDKs) will not work under it; give them the exec launcher.
type BwrapLauncher struct {
	// Path of the bwrap binary (default: looked up on PATH).
	Path string
	// ShareNet keeps the host network namespace.
	ShareNet bool
}

// Name implements Launcher.
func (BwrapLauncher) Name() string { return "bwrap" }

// Command implements Launcher.
func (b BwrapLauncher) Command(spec LaunchSpec) (*exec.Cmd, error) {
	bin := b.Path
	if bin == "" {
		p, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, fmt.Errorf("bwrap launcher: bubblewrap is not installed: %w", err)
		}
		bin = p
	}
	args := []string{
		"--die-with-parent", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup-try",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
	}
	if !b.ShareNet {
		args = append(args, "--unshare-net")
	}
	for _, d := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc/ssl", "/etc/ca-certificates", "/etc/alternatives", "/etc/ld.so.cache", "/etc/localtime"} {
		if _, err := os.Stat(d); err == nil {
			args = append(args, "--ro-bind", d, d)
		}
	}
	// Symlinked system dirs (usr-merge) are recreated by --symlink when
	// /bin etc. are links on the host.
	args = append(args, "--ro-bind", spec.PluginDir, spec.PluginDir)
	seen := map[string]bool{spec.PluginDir: true}
	for _, p := range spec.ReadOnlyPaths {
		if p = filepath.Clean(p); p != "" && !seen[p] {
			seen[p] = true
			args = append(args, "--ro-bind", p, p)
		}
	}
	args = append(args, "--bind", spec.RunDir, spec.RunDir, "--chdir", spec.PluginDir, "--")
	args = append(args, "/bin/sh", "-c", shimScript(spec.Limits), "opengtm-plugin")
	args = append(args, spec.Argv...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = spec.PluginDir
	cmd.Env = spec.Env
	cmd.ExtraFiles = []*os.File{spec.ControlFile}
	return cmd, nil
}
