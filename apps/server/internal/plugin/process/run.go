//go:build unix

package process

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

const (
	maxLogLines     = 200
	maxLogBytes     = 4096
	progressMinGap  = 250 * time.Millisecond
	stderrTailBytes = 4096
	maxFetchWorkers = 4
	// memoryHeadroomMB is added to limits.memory_mb for RLIMIT_AS: address
	// space counts reserved but untouched mappings (thread stacks, malloc
	// arenas, the interpreter), so a plugin that stays within memory_mb of
	// real data needs some slack to start at all.
	memoryHeadroomMB = 48
)

func asError(err error, target **Error) bool { return errors.As(err, target) }

// runner is one plugin run.
type runner struct {
	s      *Supervisor
	p      *manifest.Plugin
	req    Request
	argv   []string
	cancel context.CancelFunc

	token   string
	secrets map[string]string // declared secrets with values

	writeMu sync.Mutex
	conn    net.Conn
	maxFrm  int

	fmu      sync.Mutex
	fetches  []FetchEvidence
	inflight int // fetches started and not yet finished, counted against the budget

	logLines  atomic.Int64
	lastProg  time.Time
	reaped    atomic.Bool
	tail      *tailWriter
	sdk       SDKInfo
	pgid, pid int

	proxyURL string
	proxyRun *proxyRun
}

type frameMsg struct {
	body []byte
	err  error
}

// terminal is the plugin's final message.
type terminal struct {
	result  *Result
	failure *Failure
}

func (r *runner) redact(s string) string {
	for _, v := range r.secrets {
		if len(v) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, v, "REDACTED")
		if q := url.QueryEscape(v); q != v {
			s = strings.ReplaceAll(s, q, "REDACTED")
		}
	}
	return s
}

func (r *runner) send(msg any) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_ = r.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if t := r.s.opts.Trace; t != nil {
		if b, err := json.Marshal(msg); err == nil {
			t("out", b)
		}
	}
	return WriteFrame(r.conn, msg, r.maxFrm)
}

// buildEnv returns the complete child environment. Nothing is inherited: the
// worker's environment holds DATABASE_URL and every workspace secret.
func (r *runner) buildEnv(dir string) []string {
	o := &r.s.opts
	path := "/usr/local/bin:/usr/bin:/bin"
	if d := filepath.Dir(r.argv[0]); !strings.HasPrefix(path, d+":") && d != "/usr/bin" && d != "/bin" && d != "/usr/local/bin" {
		path = d + ":" + path
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PYTHONUNBUFFERED=1",
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONHASHSEED=random",
		"MALLOC_ARENA_MAX=2",
		"OPENGTM_PLUGIN_FD=3",
		"OPENGTM_PLUGIN_PROTOCOL=" + strconv.Itoa(ProtocolVersion),
		"OPENGTM_PLUGIN_DIR=" + r.p.Dir,
		MarkerEnv + "=" + r.token,
	}
	if r.proxyURL != "" {
		env = append(env, "HTTPS_PROXY="+r.proxyURL, "https_proxy="+r.proxyURL, "HTTP_PROXY="+r.proxyURL, "http_proxy="+r.proxyURL, "NO_PROXY=", "no_proxy=")
	}
	if len(o.PythonPath) > 0 {
		env = append(env, "PYTHONPATH="+strings.Join(o.PythonPath, string(os.PathListSeparator)))
	}
	for _, k := range append([]string{"SSL_CERT_FILE", "SSL_CERT_DIR", "TZ"}, o.PassEnv...) {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (r *runner) limits() ResourceLimits {
	l := r.p.Limits
	return ResourceLimits{
		AddressSpaceBytes: uint64(l.MemoryMB+memoryHeadroomMB) << 20,
		CPUSeconds:        int(l.TimeoutSeconds) + 6,
		NoFile:            r.s.opts.NoFile,
	}
}

// exec runs the plugin and returns its outcome. runCtx is cancelled by the
// caller's ctx, by Supervisor.Close and when exec returns; parent is the
// caller's own ctx (to tell shutdown from cancellation).
func (r *runner) exec(runCtx, parent context.Context) (out *Outcome, err error) {
	s := r.s
	o := &s.opts
	r.token = randHex(16)
	r.maxFrm = o.MaxFrameBytes
	r.secrets = map[string]string{}
	for _, name := range r.p.Capabilities.Secrets {
		if v, ok := r.req.Secrets[name]; ok && v != "" {
			r.secrets[name] = v
		}
	}
	runID := r.req.RunID
	if runID == "" {
		runID = randHex(6)
	}

	dir := filepath.Join(s.supDir, "r-"+randHex(6))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, &Error{Code: CodeLaunch, Message: "cannot create run directory: " + err.Error()}
	}
	defer os.RemoveAll(dir)

	if tp, run, u := s.tunnelFor(r.p); tp != nil {
		r.proxyRun, r.proxyURL = run, u
		defer tp.unregister(run)
	}

	conn, childEnd, err := socketPair()
	if err != nil {
		return nil, &Error{Code: CodeLaunch, Message: err.Error()}
	}
	r.conn = conn
	defer conn.Close()

	cmd, err := s.launcher.Command(LaunchSpec{
		Argv: r.argv, PluginDir: r.p.Dir, RunDir: dir, Env: r.buildEnv(dir), ControlFile: childEnd,
		Limits: r.limits(), ReadOnlyPaths: append(append([]string(nil), o.PythonPath...), o.ReadOnlyPaths...),
	})
	if err != nil {
		_ = childEnd.Close()
		return nil, &Error{Code: CodeLaunch, Message: err.Error()}
	}
	prepareCmd(cmd, !o.DisablePdeathsig)
	r.tail = newTailWriter(stderrTailBytes, func(line string) { r.pluginLog("info", "stdout", line) })
	cmd.Stdout, cmd.Stderr = r.tail, r.tail
	cmd.WaitDelay = 500 * time.Millisecond

	if err := s.start(cmd); err != nil {
		_ = childEnd.Close()
		return nil, &Error{Code: CodeLaunch, Message: "cannot start plugin: " + r.redact(err.Error())}
	}
	_ = childEnd.Close() // the child holds its own copy
	r.pid = cmd.Process.Pid
	r.pgid = r.pid
	start, _ := procStart(r.pid)
	_ = writeJSONFile(filepath.Join(s.supDir, filepath.Base(dir)+".json"),
		runFile{PID: r.pid, PGID: r.pgid, Start: start, Token: r.token, Plugin: r.p.Name})
	defer os.Remove(filepath.Join(s.supDir, filepath.Base(dir)+".json"))

	exitedCh := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		r.reaped.Store(true)
		close(exitedCh)
	}()
	// Whatever happens, no member of the tree outlives the run.
	defer func() { killTree(r.pgid, r.token, r.reaped.Load()) }()

	frames := make(chan frameMsg)
	done := make(chan struct{})
	defer close(done)
	go func() {
		br := bufio.NewReaderSize(conn, 64<<10)
		for {
			body, err := ReadFrame(br, r.maxFrm)
			select {
			case frames <- frameMsg{body, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var fetchWG sync.WaitGroup
	fetchSem := make(chan struct{}, maxFetchWorkers)
	defer func() {
		_ = conn.Close() // unblock fetch writes
		fetchWG.Wait()
	}()

	timeout := time.Duration(r.p.Limits.TimeoutSeconds * float64(time.Second))
	runTimer := time.NewTimer(timeout)
	defer runTimer.Stop()
	hsTimer := time.NewTimer(o.HandshakeTimeout)
	defer hsTimer.Stop()

	// stop asks the plugin to cancel, gives it CancelGrace and kills the tree.
	stop := func(reason string) {
		_ = r.send(Cancel{Type: TypeCancel, Reason: reason, GraceMS: int(o.CancelGrace / time.Millisecond)})
		select {
		case <-exitedCh:
		case <-time.After(o.CancelGrace):
		}
		killTree(r.pgid, r.token, r.reaped.Load())
	}
	crash := func(reason string) *Error {
		e := &Error{Code: CodeCrashed, Retryable: true, Message: reason}
		e.Stderr = r.redact(r.tail.String())
		if e.Stderr != "" {
			e.Message += "; last output: " + oneLine(e.Stderr, 400)
		}
		if strings.Contains(e.Stderr, "opengtm_sdk") && strings.Contains(e.Stderr, "ModuleNotFoundError") {
			e.Message += " (install the SDK with `pip install opengtm-sdk` in the plugin's interpreter, " +
				"or point OPENGTM_PLUGIN_PYTHONPATH at packages/sdk-python/src and OPENGTM_PLUGIN_PYTHON at the interpreter)"
		}
		return e
	}
	protocolErr := func(format string, args ...any) *Error {
		killTree(r.pgid, r.token, r.reaped.Load())
		return &Error{Code: CodeProtocol, Message: fmt.Sprintf(format, args...), Stderr: r.redact(r.tail.String())}
	}

	var (
		helloed  bool
		term     terminal
		recs     []Record
		outBytes int64
		hasExit  bool
	)
	exitWatch := exitedCh // set to nil once the exit has been observed
	waitExit := func() {  // wait (bounded) for the process to end, killing it if it will not
		if hasExit {
			return
		}
		select {
		case <-exitedCh:
			hasExit = true
		case <-time.After(o.ExitGrace):
			killTree(r.pgid, r.token, r.reaped.Load())
			<-exitedCh
			hasExit = true
		}
	}

loop:
	for term.result == nil && term.failure == nil {
		select {
		case <-runCtx.Done():
			stop("cancelled")
			if parent.Err() == nil {
				return nil, fmt.Errorf("process: plugin run cancelled by shutdown: %w", runCtx.Err())
			}
			return nil, fmt.Errorf("process: plugin run cancelled: %w", parent.Err())
		case <-runTimer.C:
			stop("timeout")
			return nil, &Error{Code: CodeTimeout, Retryable: true,
				Message: fmt.Sprintf("plugin exceeded limits.timeout_seconds (%gs) and was killed", r.p.Limits.TimeoutSeconds),
				Stderr:  r.redact(r.tail.String())}
		case <-hsTimer.C:
			if !helloed {
				killTree(r.pgid, r.token, r.reaped.Load())
				return nil, crash(fmt.Sprintf("plugin did not send hello within %s", o.HandshakeTimeout))
			}
		case <-exitWatch:
			// The process ended. Frames already written are still in the
			// socket; drain them before concluding anything.
			hasExit = true
			exitWatch = nil
			_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		case m := <-frames:
			if m.err != nil {
				if errors.Is(m.err, io.EOF) || isTimeout(m.err) || errors.Is(m.err, net.ErrClosed) {
					waitExit()
					return nil, crash(r.exitReason(exitErr, helloed))
				}
				return nil, protocolErr("%v", m.err)
			}
			if o.Trace != nil {
				o.Trace("in", m.body)
			}
			typ, err := frameType(m.body)
			if err != nil {
				return nil, protocolErr("%v", err)
			}
			if !helloed {
				if typ != TypeHello {
					return nil, protocolErr("first frame must be hello, got %q", typ)
				}
				var h Hello
				if err := decodeStrict(m.body, &h); err != nil {
					return nil, protocolErr("hello: %v", err)
				}
				if !containsInt(h.Protocols, ProtocolVersion) {
					killTree(r.pgid, r.token, r.reaped.Load())
					return nil, &Error{Code: CodeProtocol, Message: fmt.Sprintf(
						"plugin supports protocol versions %v, this host speaks %d; upgrade opengtm-sdk", h.Protocols, ProtocolVersion)}
				}
				r.sdk = h.SDK
				if err := r.send(r.initFrame(runID, timeout)); err != nil {
					if errors.Is(err, ErrFrameTooLarge) {
						killTree(r.pgid, r.token, r.reaped.Load())
						return nil, &Error{Code: CodeInvalidInput, Message: "run inputs and configuration do not fit one protocol frame: " + err.Error()}
					}
					return nil, crash("cannot send init: " + err.Error())
				}
				helloed = true
				continue
			}
			switch typ {
			case TypeFetch:
				var f Fetch
				if err := decodeStrict(m.body, &f); err != nil {
					return nil, protocolErr("fetch: %v", err)
				}
				select {
				case fetchSem <- struct{}{}:
				case <-runCtx.Done():
					continue
				}
				fetchWG.Add(1)
				go func() {
					defer fetchWG.Done()
					defer func() { <-fetchSem }()
					res := r.doFetch(runCtx, f)
					if err := r.send(res); err != nil && runCtx.Err() == nil {
						s.log.Debug("plugin fetch reply failed", "plugin", r.p.Name, "err", err)
					}
				}()
			case TypeProgress:
				var pr Progress
				if err := decodeStrict(m.body, &pr); err != nil {
					return nil, protocolErr("progress: %v", err)
				}
				if r.req.OnProgress != nil && time.Since(r.lastProg) >= progressMinGap {
					r.lastProg = time.Now()
					pr.Message = r.redact(pr.Message)
					r.req.OnProgress(pr)
				}
			case TypeLog:
				var l Log
				if err := decodeStrict(m.body, &l); err != nil {
					return nil, protocolErr("log: %v", err)
				}
				r.pluginLog(l.Level, "sdk", l.Message)
			case TypeRecords:
				var rs Records
				if err := decodeStrict(m.body, &rs); err != nil {
					return nil, protocolErr("records: %v", err)
				}
				outBytes += int64(len(m.body))
				recs = append(recs, rs.Records...)
				if len(recs) > o.MaxRecords || outBytes > o.MaxOutputBytes {
					killTree(r.pgid, r.token, r.reaped.Load())
					return nil, &Error{Code: CodeOutputTooLarge, Message: fmt.Sprintf(
						"plugin returned more than %d records or %d bytes", o.MaxRecords, o.MaxOutputBytes)}
				}
			case TypeResult:
				var res Result
				if err := decodeStrict(m.body, &res); err != nil {
					return nil, protocolErr("result: %v", err)
				}
				outBytes += int64(len(m.body))
				term.result = &res
			case TypeFailure:
				var f Failure
				if err := decodeStrict(m.body, &f); err != nil {
					return nil, protocolErr("failure: %v", err)
				}
				term.failure = &f
			default:
				return nil, protocolErr("unexpected %q frame from plugin", typ)
			}
			continue loop
		}
	}

	// Terminal message received. Let the plugin exit by itself; the deferred
	// killTree then removes anything it left behind.
	waitExit()

	if f := term.failure; f != nil {
		retry := retryableCode(f.Code)
		if f.Retryable != nil {
			retry = *f.Retryable
		}
		return nil, &Error{Code: f.Code, Message: r.redact(f.Message), Retryable: retry, FromPlugin: true,
			Details: redactMap(f.Details, r.redact), Stderr: r.redact(r.tail.String())}
	}
	return r.finish(term.result, recs)
}

func (r *runner) initFrame(runID string, timeout time.Duration) Init {
	secrets := make(map[string]string, len(r.secrets))
	for k, v := range r.secrets {
		secrets[k] = v
	}
	l := r.p.Limits
	return Init{
		Type: TypeInit, Protocol: ProtocolVersion, RunID: runID,
		Plugin:  InitPlugin{Name: r.p.Name, Version: r.p.Version, Kind: r.p.Kind},
		Inputs:  nonNil(r.req.Inputs),
		Config:  r.req.Config,
		Secrets: secrets,
		Limits: InitLimits{TimeoutSeconds: l.TimeoutSeconds, MaxPages: l.MaxPages, MaxResponseBytes: l.MaxResponseBytes,
			MemoryMB: l.MemoryMB, MaxFrameBytes: r.maxFrm},
		DeadlineUnixMS: time.Now().Add(timeout).UnixMilli(),
		Proxy:          proxyInit(r.proxyURL),
	}
}

func proxyInit(u string) *InitProxy {
	if u == "" {
		return nil
	}
	return &InitProxy{URL: u}
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// exitReason describes how the process ended without a final message.
func (r *runner) exitReason(err error, helloed bool) string {
	switch {
	case err == nil:
		if !helloed {
			return "plugin exited before sending hello (is opengtm_sdk installed? set OPENGTM_PLUGIN_PYTHONPATH or OPENGTM_PLUGIN_PYTHON)"
		}
		return "plugin exited successfully without sending a result"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := exitStatus(ee); ok {
			if ws.Signaled() {
				return fmt.Sprintf("plugin was killed by signal %s", ws.Signal())
			}
			return fmt.Sprintf("plugin exited with status %d", ws.ExitStatus())
		}
	}
	return "plugin failed: " + err.Error()
}

// finish validates a successful result and merges host evidence.
func (r *runner) finish(res *Result, streamed []Record) (*Outcome, error) {
	o := &r.s.opts
	records := append(streamed, res.Records...)
	if len(records) > o.MaxRecords {
		return nil, &Error{Code: CodeOutputTooLarge, Message: fmt.Sprintf("plugin returned %d records, limit %d", len(records), o.MaxRecords)}
	}
	if r.p.Kind == "provider" && len(records) > 1 {
		return nil, &Error{Code: CodeInvalidOutput, Message: fmt.Sprintf("a provider returns at most one record, got %d", len(records))}
	}
	if schema, ok := r.p.Outputs.(map[string]any); ok && len(records) > 0 {
		rows := make([]any, len(records))
		for i, rec := range records {
			if rec.Fields == nil {
				return nil, &Error{Code: CodeInvalidOutput, Message: fmt.Sprintf("record %d has no fields object", i)}
			}
			rows[i] = rec.Fields
		}
		if err := manifest.ValidateValue(map[string]any{"type": "array", "items": schema}, rows); err != nil {
			return nil, &Error{Code: CodeInvalidOutput, Message: "records do not match the manifest's outputs schema: " + oneLine(err.Error(), 600)}
		}
	}
	r.fmu.Lock()
	fetches := append([]FetchEvidence(nil), r.fetches...)
	r.fmu.Unlock()

	var tunnels []TunnelEvidence
	if r.proxyRun != nil {
		tunnels = r.proxyRun.evidence()
	}
	out := &Outcome{Fetches: fetches, Tunnels: tunnels, SDK: r.sdk, Stopped: r.redact(res.Stopped), ProviderError: r.redact(res.ProviderError)}
	if res.Pages != nil {
		out.Pages = *res.Pages
	} else {
		out.Pages = len(fetches)
	}
	switch {
	case res.CostUSD != nil:
		out.CostUSD = *res.CostUSD
	case r.p.Provider != nil && r.p.Provider.CostPerLookup > 0 && len(records) > 0:
		out.CostUSD = r.p.Provider.CostPerLookup
	}
	if out.CostUSD < 0 {
		return nil, &Error{Code: CodeInvalidOutput, Message: "cost_usd must not be negative"}
	}
	for i, rec := range records {
		if rec.Fields == nil {
			return nil, &Error{Code: CodeInvalidOutput, Message: fmt.Sprintf("record %d has no fields object", i)}
		}
		ev := map[string]any{
			"runtime": "process",
			"fetches": fetches,
			"plugin":  redactAny(rec.Evidence, r.redact),
		}
		if len(tunnels) > 0 {
			ev["tunnels"] = tunnels
		}
		out.Records = append(out.Records, OutRecord{Fields: redactMap(rec.Fields, r.redact), Evidence: ev})
	}
	return out, nil
}

// pluginLog forwards one plugin log line, bounded and redacted.
func (r *runner) pluginLog(level, source, msg string) {
	if r.logLines.Add(1) > maxLogLines {
		return
	}
	if len(msg) > maxLogBytes {
		msg = msg[:maxLogBytes]
		for !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	msg = r.redact(msg)
	attrs := []any{"plugin", r.p.Name, "source", source}
	switch strings.ToLower(level) {
	case "debug", "trace":
		r.s.log.Debug(msg, attrs...)
	case "warn", "warning":
		r.s.log.Warn(msg, attrs...)
	case "error":
		r.s.log.Error(msg, attrs...)
	default:
		r.s.log.Info(msg, attrs...)
	}
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// tailWriter keeps the last n bytes written and reports complete lines.
type tailWriter struct {
	mu     sync.Mutex
	n      int
	buf    []byte
	line   []byte
	onLine func(string)
}

func newTailWriter(n int, onLine func(string)) *tailWriter {
	return &tailWriter{n: n, onLine: onLine}
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = t.buf[len(t.buf)-t.n:]
	}
	var lines []string
	for _, b := range p {
		if b == '\n' {
			lines = append(lines, string(t.line))
			t.line = t.line[:0]
		} else if len(t.line) < maxLogBytes {
			t.line = append(t.line, b)
		}
	}
	t.mu.Unlock()
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			t.onLine(l)
		}
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(strings.ToValidUTF8(string(t.buf), "?"))
}
