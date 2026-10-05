//go:build unix

package process

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

// Supervisor starts plugin processes, one per run, under bounded concurrency.
// A process serves exactly one run and is never reused, so nothing a plugin
// holds in memory or on disk can leak from one run (or one workspace) into
// the next. Create one per worker process and share it.
type Supervisor struct {
	opts     Options
	log      Logger
	launcher Launcher

	global chan struct{}

	mu        sync.Mutex
	perPlugin map[string]chan struct{}
	closed    bool
	active    map[*runner]struct{}
	stats     Stats

	supDir string
	spawn  chan spawnReq
	wg     sync.WaitGroup

	tunnelOnce sync.Once
	tunnel     *tunnelProxy
}

type spawnReq struct {
	cmd  *exec.Cmd
	done chan error
}

// NewSupervisor builds a Supervisor, reclaims what dead supervisors left in
// the state directory (see Sweep) and hardens the host process.
func NewSupervisor(opts Options) (*Supervisor, error) {
	if opts.MaxProcesses <= 0 {
		opts.MaxProcesses = min(DefaultMaxProcesses, max(1, runtime.NumCPU()))
	}
	if opts.MaxPerPlugin <= 0 {
		opts.MaxPerPlugin = max(1, (opts.MaxProcesses+1)/2)
	}
	opts.MaxPerPlugin = min(opts.MaxPerPlugin, opts.MaxProcesses)
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&opts.QueueWait, DefaultQueueWait)
	def(&opts.CancelGrace, DefaultCancelGrace)
	def(&opts.ExitGrace, DefaultExitGrace)
	def(&opts.HandshakeTimeout, DefaultHandshakeTimeout)
	if opts.NoFile <= 0 {
		opts.NoFile = DefaultNoFile
	}
	if opts.MaxFrameBytes <= 0 {
		opts.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = DefaultMaxRecords
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Launcher == nil {
		opts.Launcher = ExecLauncher{}
	}
	if opts.StateDir == "" {
		opts.StateDir = defaultStateDir()
	}
	if err := prepareStateDir(opts.StateDir); err != nil {
		return nil, err
	}
	if res, err := Sweep(opts.StateDir, opts.Logger); err != nil {
		opts.Logger.Warn("plugin state sweep failed", "err", err)
	} else if res.Supervisors > 0 {
		opts.Logger.Warn("reclaimed plugin state from dead supervisors", "supervisors", res.Supervisors,
			"killed_processes", res.Killed, "run_dirs", res.Removed)
	}
	s := &Supervisor{
		opts:      opts,
		log:       opts.Logger,
		launcher:  opts.Launcher,
		global:    make(chan struct{}, opts.MaxProcesses),
		perPlugin: map[string]chan struct{}{},
		active:    map[*runner]struct{}{},
		spawn:     make(chan spawnReq),
	}
	s.supDir = filepath.Join(opts.StateDir, fmt.Sprintf("sup-%d-%s", os.Getpid(), randHex(4)))
	if err := os.Mkdir(s.supDir, 0o700); err != nil {
		return nil, err
	}
	start, _ := procStart(os.Getpid())
	if err := writeJSONFile(filepath.Join(s.supDir, "owner.json"), ownerFile{PID: os.Getpid(), Start: start}); err != nil {
		_ = os.RemoveAll(s.supDir)
		return nil, err
	}
	if !opts.KeepParentDumpable {
		if err := hardenParent(); err != nil {
			s.log.Warn("cannot clear PR_SET_DUMPABLE; plugins running as this user may read the worker's /proc entries", "err", err)
		}
	}
	go s.spawner()
	return s, nil
}

// spawner starts every plugin process from one dedicated OS thread. With
// PR_SET_PDEATHSIG the kernel kills the child when the *thread* that created
// it exits, and Go may retire idle threads, so all launches share a thread
// that lives exactly as long as the supervisor (closing the supervisor, or
// the worker dying, then also kills any process still running).
func (s *Supervisor) spawner() {
	runtime.LockOSThread()
	for req := range s.spawn {
		req.done <- req.cmd.Start()
	}
}

func (s *Supervisor) start(cmd *exec.Cmd) error {
	req := spawnReq{cmd: cmd, done: make(chan error, 1)}
	s.spawn <- req
	return <-req.done
}

// Close cancels in-flight runs (killing their process trees), waits for them
// and removes the supervisor's state. The supervisor is unusable afterwards.
func (s *Supervisor) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for r := range s.active {
		r.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if s.tunnel != nil {
		s.tunnel.close()
	}
	close(s.spawn)
	_ = os.RemoveAll(s.supDir)
}

// Stats returns a snapshot of pool activity.
func (s *Supervisor) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *Supervisor) count(f func(*Stats)) {
	s.mu.Lock()
	f(&s.stats)
	s.mu.Unlock()
}

// acquire takes a global slot and a per-plugin slot, waiting at most
// QueueWait. Waiting is bounded so a saturated pool turns into a retryable
// error (and the queue's backoff) instead of parking workers forever.
func (s *Supervisor) acquire(ctx context.Context, name string) (func(), error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("process: supervisor is closed")
	}
	pp, ok := s.perPlugin[name]
	if !ok {
		pp = make(chan struct{}, s.opts.MaxPerPlugin)
		s.perPlugin[name] = pp
	}
	s.stats.Waiting++
	s.mu.Unlock()

	timer := time.NewTimer(s.opts.QueueWait)
	defer timer.Stop()
	fail := func(err error) (func(), error) {
		s.count(func(st *Stats) { st.Waiting--; st.Rejected++ })
		return nil, err
	}
	busy := &Error{Code: CodeBusy, Retryable: true,
		Message: fmt.Sprintf("no plugin process slot became free within %s (max %d processes, %d per plugin)",
			s.opts.QueueWait, s.opts.MaxProcesses, s.opts.MaxPerPlugin)}
	select {
	case pp <- struct{}{}:
	case <-timer.C:
		return fail(busy)
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	select {
	case s.global <- struct{}{}:
	case <-timer.C:
		<-pp
		return fail(busy)
	case <-ctx.Done():
		<-pp
		return fail(ctx.Err())
	}
	s.count(func(st *Stats) {
		st.Waiting--
		st.Running++
		st.Started++
		st.PeakRunning = max(st.PeakRunning, st.Running)
	})
	return func() {
		<-s.global
		<-pp
		s.count(func(st *Stats) { st.Running-- })
	}, nil
}

// Run executes the plugin once. The returned error is a *Error for plugin
// and process failures (Retryable tells the queue what to do), or wraps the
// context error when ctx ended the run.
func (s *Supervisor) Run(ctx context.Context, p *manifest.Plugin, req Request) (*Outcome, error) {
	if p == nil || p.Runtime != "process" || p.Process == nil || len(p.Process.Command) == 0 {
		return nil, &Error{Code: CodeLaunch, Message: "not a process plugin"}
	}
	argv, err := s.resolveCommand(p)
	if err != nil {
		return nil, &Error{Code: CodeLaunch, Message: err.Error()}
	}
	release, err := s.acquire(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	defer release()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := &runner{s: s, p: p, req: req, argv: argv, cancel: cancel}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("process: supervisor is closed")
	}
	s.active[r] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, r)
		s.mu.Unlock()
		s.wg.Done()
	}()

	out, err := r.exec(runCtx, ctx)
	s.count(func(st *Stats) {
		var pe *Error
		switch {
		case err == nil:
			st.Succeeded++
		case asError(err, &pe) && pe.Code == CodeTimeout:
			st.TimedOut++
			st.Failed++
		case asError(err, &pe) && pe.Code == CodeCrashed:
			st.Crashed++
			st.Failed++
		case ctx.Err() != nil:
			st.Cancelled++
		default:
			st.Failed++
		}
	})
	return out, err
}

// tunnelFor registers a run with the tunnel proxy when the plugin may use one:
// the proxy is enabled, there is an egress client to dial through, the plugin
// declares network access, the launcher leaves a network to proxy over, and a
// scraper or signal declared capabilities.browser (tunnels bypass robots.txt
// and rate limits, so only plugins that say they drive a browser get one).
func (s *Supervisor) tunnelFor(p *manifest.Plugin) (*tunnelProxy, *proxyRun, string) {
	if s.opts.DisableProxy || s.opts.Client == nil || len(p.Capabilities.Network) == 0 {
		return nil, nil, ""
	}
	if nl, ok := s.launcher.(networkLauncher); ok && !nl.HasNetwork() {
		return nil, nil, ""
	}
	if (p.Kind == "scraper" || p.Kind == "signal") && !p.Capabilities.Browser {
		return nil, nil, ""
	}
	s.tunnelOnce.Do(func() {
		tp, err := newTunnelProxy(s.opts.Client)
		if err != nil {
			s.log.Warn("cannot start the plugin tunnel proxy; plugins must use ctx.fetch", "err", err)
			return
		}
		s.mu.Lock()
		s.tunnel = tp
		s.mu.Unlock()
	})
	s.mu.Lock()
	tp := s.tunnel
	s.mu.Unlock()
	if tp == nil {
		return nil, nil, ""
	}
	run, u := tp.register(p)
	return tp, run, u
}

// resolveCommand turns the manifest command into an argv whose first element
// is an absolute path. A bare name is looked up on the host PATH (python and
// python3 may be overridden by Options.Python); a path with a slash is
// relative to the plugin directory unless absolute.
func (s *Supervisor) resolveCommand(p *manifest.Plugin) ([]string, error) {
	cmd := append([]string(nil), p.Process.Command...)
	exe := cmd[0]
	if (exe == "python" || exe == "python3") && s.opts.Python != "" {
		exe = s.opts.Python
	}
	switch {
	case strings.ContainsRune(exe, '/'):
		if !filepath.IsAbs(exe) {
			if err := manifest.SafeRelPath(strings.TrimPrefix(exe, "./")); err != nil {
				return nil, fmt.Errorf("process.command[0]: %w", err)
			}
			exe = filepath.Join(p.Dir, exe)
		}
	default:
		path, err := exec.LookPath(exe)
		if err != nil {
			return nil, fmt.Errorf("process.command[0] %q not found on PATH (set OPENGTM_PLUGIN_PYTHON for Python plugins): %w", exe, err)
		}
		exe = path
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return nil, err
	}
	cmd[0] = abs
	return cmd, nil
}
