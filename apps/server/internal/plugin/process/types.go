package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
)

// Defaults for Options.
const (
	DefaultMaxProcesses     = 4
	DefaultQueueWait        = 30 * time.Second
	DefaultCancelGrace      = time.Second
	DefaultExitGrace        = 2 * time.Second
	DefaultHandshakeTimeout = 30 * time.Second
	DefaultMaxRecords       = 50_000
	DefaultMaxOutputBytes   = 32 << 20
	DefaultNoFile           = 256
)

// ErrUnsupported is returned on platforms without POSIX process groups and
// unix sockets (Windows). Process plugins need Linux or macOS.
var ErrUnsupported = errors.New("process plugins require a unix host (Linux or macOS)")

// Options configures a Supervisor. The zero value works: it picks the
// documented defaults, launches with the "exec" launcher and uses no egress
// client (so fetch requests fail with network_unavailable).
type Options struct {
	// MaxProcesses bounds plugin processes alive at once across all plugins
	// (default min(4, CPUs)).
	MaxProcesses int
	// MaxPerPlugin bounds processes of one plugin, so a busy plugin cannot
	// take every slot (default half of MaxProcesses, at least 1).
	MaxPerPlugin int
	// QueueWait is how long a run waits for a free slot before failing with
	// the retryable pool_busy error (default 30s).
	QueueWait time.Duration
	// CancelGrace is how long a plugin gets after a cancel frame before the
	// process group is killed (default 1s).
	CancelGrace time.Duration
	// ExitGrace is how long a plugin gets to exit after sending its final
	// message (default 2s).
	ExitGrace time.Duration
	// HandshakeTimeout bounds start-up until the plugin's hello (default 30s).
	HandshakeTimeout time.Duration

	// Client performs plugin fetches. Its destination guard, robots.txt,
	// rate limits, size caps and OPENGTM_EGRESS_PROXY apply to every fetch.
	Client *egress.Client
	// Logger receives supervisor and plugin log lines.
	Logger Logger

	// Launcher starts plugin processes (default: ExecLauncher). BwrapLauncher
	// is a stronger, optional sandbox.
	Launcher Launcher
	// StateDir holds per-supervisor run directories and the crash-recovery
	// registry (default $TMPDIR/opengtm-plugins-<uid>, mode 0700).
	StateDir string
	// Python overrides the interpreter when a manifest's command starts with
	// python or python3 (for example a virtualenv with the SDK installed).
	Python string
	// PythonPath entries are put on PYTHONPATH so plugins find opengtm_sdk
	// without it being installed (development checkouts).
	PythonPath []string
	// PassEnv names additional host environment variables to pass through.
	// Everything not listed here, and not in the fixed allow-list, is
	// removed. Never list secrets: declared secrets travel in the init frame.
	PassEnv []string
	// ReadOnlyPaths are extra host paths a sandboxing launcher must expose
	// read-only (interpreter prefix, SDK checkout).
	ReadOnlyPaths []string

	// NoFile is the open-file limit (default 256).
	NoFile int
	// MaxFrameBytes bounds one wire frame (default 16 MiB).
	MaxFrameBytes int
	// MaxRecords and MaxOutputBytes bound what one run may return.
	MaxRecords     int
	MaxOutputBytes int64

	// KeepParentDumpable leaves the host process dumpable. By default the
	// supervisor clears PR_SET_DUMPABLE on Linux so a plugin running as the
	// same user cannot read /proc/<host>/environ, /proc/<host>/mem or ptrace
	// the worker (which holds every workspace's secrets).
	KeepParentDumpable bool
	// DisablePdeathsig stops the kernel from killing plugin processes when
	// the supervisor dies (used by recovery tests).
	DisablePdeathsig bool
	// Trace observes every frame ("in" is plugin to host, "out" host to
	// plugin). Tests use it for contract checks; secrets are NOT redacted.
	Trace func(dir string, frame []byte)
}

// Logger is the subset of slog the supervisor needs.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	Debug(msg string, args ...any)
}

// OptionsFromEnv layers OPENGTM_PLUGIN_* environment variables over base:
//
//	OPENGTM_PLUGIN_MAX_PROCESSES   total concurrent plugin processes
//	OPENGTM_PLUGIN_MAX_PER_PLUGIN  concurrent processes of one plugin
//	OPENGTM_PLUGIN_PYTHON          interpreter for python/python3 commands
//	OPENGTM_PLUGIN_PYTHONPATH      path list added to PYTHONPATH
//	OPENGTM_PLUGIN_STATE_DIR       run directories and crash registry
//	OPENGTM_PLUGIN_SANDBOX         "exec" (default) or "bwrap"
//	OPENGTM_PLUGIN_SANDBOX_RO      extra path list bound read-only (bwrap)
func OptionsFromEnv(base Options, lookup func(string) (string, bool)) (Options, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(k string) string {
		v, _ := lookup(k)
		return strings.TrimSpace(v)
	}
	atoi := func(k string, dst *int) error {
		if v := get(k); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 256 {
				return fmt.Errorf("%s=%q must be an integer from 1 to 256", k, v)
			}
			*dst = n
		}
		return nil
	}
	if err := atoi("OPENGTM_PLUGIN_MAX_PROCESSES", &base.MaxProcesses); err != nil {
		return base, err
	}
	if err := atoi("OPENGTM_PLUGIN_MAX_PER_PLUGIN", &base.MaxPerPlugin); err != nil {
		return base, err
	}
	if v := get("OPENGTM_PLUGIN_PYTHON"); v != "" {
		base.Python = v
	}
	if v := get("OPENGTM_PLUGIN_PYTHONPATH"); v != "" {
		base.PythonPath = append(base.PythonPath, filepath.SplitList(v)...)
	}
	if v := get("OPENGTM_PLUGIN_STATE_DIR"); v != "" {
		base.StateDir = v
	}
	if v := get("OPENGTM_PLUGIN_SANDBOX_RO"); v != "" {
		base.ReadOnlyPaths = append(base.ReadOnlyPaths, filepath.SplitList(v)...)
	}
	switch v := strings.ToLower(get("OPENGTM_PLUGIN_SANDBOX")); v {
	case "", "exec":
	case "bwrap":
		base.Launcher = BwrapLauncher{}
	default:
		return base, fmt.Errorf("OPENGTM_PLUGIN_SANDBOX=%q must be exec or bwrap", v)
	}
	return base, nil
}

// Request is one plugin run.
type Request struct {
	// RunID labels logs and the init frame (any short string).
	RunID string
	// Inputs, already validated against the manifest.
	Inputs map[string]any
	// Config is the plugin's workspace configuration (optional).
	Config map[string]any
	// Secrets holds values for declared secrets. Names the manifest does not
	// declare are ignored: a plugin never receives a secret it did not ask
	// for in capabilities.secrets.
	Secrets map[string]string
	// OnProgress receives rate-limited progress (optional).
	OnProgress func(Progress)
}

// OutRecord is a result row with merged evidence.
type OutRecord struct {
	Fields map[string]any
	// Evidence is {"runtime":"process","fetches":[host-observed],"plugin":<plugin's own>}.
	Evidence map[string]any
}

// Outcome is a successful run.
type Outcome struct {
	Records       []OutRecord
	Pages         int
	CostUSD       float64
	Stopped       string
	ProviderError string
	Fetches       []FetchEvidence
	SDK           SDKInfo
}

// Error is a structured run failure. Retryable tells the queue whether
// another attempt could succeed.
type Error struct {
	Code      string
	Message   string
	Retryable bool
	// FromPlugin is true when the plugin itself reported the failure (a
	// failure frame), false for host-detected faults: crash, timeout,
	// protocol violation, invalid output, launch and capacity errors.
	FromPlugin bool
	Details    map[string]any
	// Stderr is the redacted tail of the plugin's output, for diagnosis.
	Stderr string
}

func (e *Error) Error() string {
	s := e.Code
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// retryableCode is the default retry semantics of a failure code.
func retryableCode(code string) bool {
	switch code {
	case CodeUpstream, CodeRateLimited, CodeTimeout, CodeCrashed, CodeBusy:
		return true
	}
	return false
}

// Stats is a snapshot of pool activity.
type Stats struct {
	Running     int    `json:"running"`
	Waiting     int    `json:"waiting"`
	PeakRunning int    `json:"peak_running"`
	Started     uint64 `json:"started"`
	Succeeded   uint64 `json:"succeeded"`
	Failed      uint64 `json:"failed"`
	Crashed     uint64 `json:"crashed"`
	TimedOut    uint64 `json:"timed_out"`
	Cancelled   uint64 `json:"cancelled"`
	Rejected    uint64 `json:"rejected"`
}
