// Package wasmhost runs WebAssembly plugins through the Extism Go SDK (pure
// Go, on wazero). Each plugin module is verified against the SHA-256 pinned
// in its manifest, compiled once, and instantiated per call from a bounded
// pool.
//
// Sandbox:
//   - memory: limits.memory_mb (64 KiB pages) for every module in the runtime;
//   - CPU: wall-clock timeout from limits.timeout_seconds (the runtime closes
//     the module when the context ends, which stops infinite loops);
//   - output: capped at Options.MaxOutputBytes;
//   - network: Extism's built-in HTTP is disabled (no allowed hosts); the only
//     way out is the opengtm_fetch host function, which enforces the network
//     capability and goes through the egress client;
//   - secrets: only the manifest's declared secrets are passed, as Extism
//     config values, resolved per call;
//   - WASI without filesystem preopens, environment or arguments.
//
// Host functions live in Extism's user namespace "extism:host/user" (the
// default import module of extism-pdk's #[host_fn]). See
// docs/plugins/wasm-abi.md for the ABI.
package wasmhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

// Exports by plugin kind.
var Exports = map[string]string{
	"provider": "enrich",
	"scraper":  "scrape",
	"function": "call",
	"tool":     "invoke",
}

// Defaults.
const (
	DefaultMaxInstances   = 4
	DefaultMaxOutputBytes = 1 << 20
	maxLogLines           = 200
	maxLogBytes           = 4096
)

// Errors.
var (
	ErrOutputTooLarge = errors.New("wasm: plugin output exceeds the limit")
	ErrTimeout        = errors.New("wasm: plugin exceeded its time limit")
	ErrHashMismatch   = errors.New("wasm: module sha256 does not match the manifest")
	ErrNoExport       = errors.New("wasm: plugin does not export the function for its kind")
)

// SecretResolver resolves a declared secret for the current workspace.
type SecretResolver interface {
	Secret(ctx context.Context, name string) (string, error)
}

// Options configures a Host.
type Options struct {
	Client         *egress.Client
	Logger         *slog.Logger
	MaxInstances   int   // per plugin (default 4)
	MaxOutputBytes int64 // default 1 MiB
}

// Host loads and runs WebAssembly plugins.
type Host struct {
	opts Options
}

// New returns a Host.
func New(opts Options) *Host {
	if opts.MaxInstances <= 0 {
		opts.MaxInstances = DefaultMaxInstances
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Host{opts: opts}
}

// Plugin is a compiled module with its instance pool.
type Plugin struct {
	host     *Host
	manifest *manifest.Plugin
	compiled *extism.CompiledPlugin
	sem      chan struct{}
	mu       sync.Mutex
	idle     []*extism.Plugin
}

// ModulePath returns the absolute path of the plugin's module.
func ModulePath(p *manifest.Plugin) (string, error) {
	if p.Wasm == nil {
		return "", errors.New("wasm: manifest has no wasm block")
	}
	if err := manifest.SafeRelPath(p.Wasm.Module); err != nil {
		return "", err
	}
	return filepath.Join(p.Dir, filepath.FromSlash(p.Wasm.Module)), nil
}

// Load reads, verifies and compiles a wasm plugin.
func (h *Host) Load(ctx context.Context, p *manifest.Plugin) (*Plugin, error) {
	if p.Runtime != "wasm" || p.Wasm == nil {
		return nil, errors.New("wasm: plugin runtime is not wasm")
	}
	path, err := ModulePath(p)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != p.Wasm.SHA256 {
		return nil, fmt.Errorf("%w (%s)", ErrHashMismatch, p.Wasm.Module)
	}
	pages := uint32(p.Limits.MemoryMB) * 16 // 64 KiB pages
	rc := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(pages)
	compiled, err := extism.NewCompiledPlugin(ctx, extism.Manifest{
		Wasm:         []extism.Wasm{extism.WasmData{Data: data, Hash: p.Wasm.SHA256, Name: "main"}},
		Memory:       &extism.ManifestMemory{MaxPages: pages, MaxHttpResponseBytes: 0, MaxVarBytes: 64 << 10},
		AllowedHosts: nil, // Extism's own HTTP stays disabled
		AllowedPaths: nil, // no filesystem
	}, extism.PluginConfig{RuntimeConfig: rc, EnableWasi: true}, h.hostFunctions())
	if err != nil {
		return nil, fmt.Errorf("wasm: compile %s: %w", p.Wasm.Module, err)
	}
	return &Plugin{host: h, manifest: p, compiled: compiled, sem: make(chan struct{}, h.opts.MaxInstances)}, nil
}

// Close releases the compiled module and pooled instances.
func (pl *Plugin) Close(ctx context.Context) error {
	pl.mu.Lock()
	for _, inst := range pl.idle {
		_ = inst.Close(ctx)
	}
	pl.idle = nil
	pl.mu.Unlock()
	return pl.compiled.Close(ctx)
}

func (pl *Plugin) acquire(ctx context.Context) (*extism.Plugin, error) {
	select {
	case pl.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	pl.mu.Lock()
	if n := len(pl.idle); n > 0 {
		inst := pl.idle[n-1]
		pl.idle = pl.idle[:n-1]
		pl.mu.Unlock()
		return inst, nil
	}
	pl.mu.Unlock()
	inst, err := pl.compiled.Instance(ctx, extism.PluginInstanceConfig{ModuleConfig: wazero.NewModuleConfig()})
	if err != nil {
		<-pl.sem
		return nil, fmt.Errorf("wasm: instantiate: %w", err)
	}
	inst.SetLogger(func(extism.LogLevel, string) {}) // guest logs go through opengtm_log
	return inst, nil
}

func (pl *Plugin) release(inst *extism.Plugin, reusable bool) {
	if reusable {
		pl.mu.Lock()
		pl.idle = append(pl.idle, inst)
		pl.mu.Unlock()
	} else {
		_ = inst.Close(context.Background())
	}
	<-pl.sem
}

// CallResult is the outcome of one export call.
type CallResult struct {
	Output  []byte          `json:"-"`
	Fetches []FetchEvidence `json:"fetches"`
	Logs    int             `json:"logs"`
}

// Call invokes an export with raw input. Only declared secrets are resolved
// and exposed as config.
func (pl *Plugin) Call(ctx context.Context, export string, input []byte, secrets SecretResolver) (*CallResult, error) {
	p := pl.manifest
	config := map[string]string{}
	values := map[string]string{}
	for _, name := range p.Capabilities.Secrets {
		if secrets == nil {
			break
		}
		v, err := secrets.Secret(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("resolve secret %s: %w", name, err)
		}
		if v != "" {
			config[name] = v
			values[name] = v
		}
	}
	timeout := time.Duration(p.Limits.TimeoutSeconds * float64(time.Second))
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	inst, err := pl.acquire(cctx)
	if err != nil {
		return nil, err
	}
	if !inst.FunctionExists(export) {
		pl.release(inst, true)
		return nil, fmt.Errorf("%w: %s", ErrNoExport, export)
	}
	inst.Config = config
	state := &callState{plugin: p, host: pl.host, secrets: values}
	cctx = context.WithValue(cctx, callKey{}, state)
	_, out, callErr := inst.CallWithContext(cctx, export, input)
	inst.Config = map[string]string{}
	timedOut := cctx.Err() != nil
	pl.release(inst, callErr == nil && !timedOut)
	res := &CallResult{Fetches: state.fetches, Logs: state.logs}
	if timedOut && ctx.Err() == nil {
		return res, fmt.Errorf("%w (%s)", ErrTimeout, timeout)
	}
	if callErr != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, fmt.Errorf("wasm: %s: %s", export, redact(callErr.Error(), values))
	}
	if int64(len(out)) > pl.host.opts.MaxOutputBytes {
		return res, fmt.Errorf("%w (%d > %d bytes)", ErrOutputTooLarge, len(out), pl.host.opts.MaxOutputBytes)
	}
	res.Output = out
	return res, nil
}

// Invocation is the JSON input every kind export receives.
type Invocation struct {
	Inputs map[string]any `json:"inputs"`
	Config map[string]any `json:"config"`
}

// Invoke validates inputs, calls the export for the plugin's kind with the
// standard JSON envelope and decodes the JSON output.
func (pl *Plugin) Invoke(ctx context.Context, inputs, config map[string]any, secrets SecretResolver) (map[string]any, *CallResult, error) {
	p := pl.manifest
	export, ok := Exports[p.Kind]
	if !ok {
		return nil, nil, fmt.Errorf("wasm: kind %s has no wasm export contract yet", p.Kind)
	}
	if err := p.ValidateInputs(inputs); err != nil {
		return nil, nil, err
	}
	if p.Config != nil {
		cfg := config
		if cfg == nil {
			cfg = map[string]any{}
		}
		if err := manifest.ValidateValue(p.Config, cfg); err != nil {
			return nil, nil, fmt.Errorf("config does not match the plugin's config schema: %w", err)
		}
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	if config == nil {
		config = map[string]any{}
	}
	in, err := json.Marshal(Invocation{Inputs: inputs, Config: config})
	if err != nil {
		return nil, nil, err
	}
	res, err := pl.Call(ctx, export, in, secrets)
	if err != nil {
		return nil, res, err
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return nil, res, fmt.Errorf("wasm: %s returned invalid JSON: %w", export, err)
	}
	return out, res, nil
}
