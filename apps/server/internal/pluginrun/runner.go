package pluginrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/process"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/wasmhost"
)

// ErrUnsupported marks a plugin this host cannot run (for example a process
// plugin on a platform without unix process groups, or a kind with no
// executor yet).
var ErrUnsupported = errors.New("pluginrun: plugin runtime not supported by this host")

// Record is one result row: normalized data plus the evidence that produced it.
type Record struct {
	Data     map[string]any `json:"data"`
	Evidence any            `json:"evidence"`
}

// Outcome is the result of one plugin run.
type Outcome struct {
	Records []Record
	Pages   int
	CostUSD float64
	// Stopped explains why a scraper ended pagination early.
	Stopped string
	// ProviderError is a provider's own "no result" or vendor error. It does
	// not fail the run: the provider answered.
	ProviderError string
}

// Runner executes plugins. One Runner is shared by every job in a worker
// process so the egress client's pooled connections, robots cache and
// per-domain limits, and the compiled WebAssembly modules, are reused.
type Runner struct {
	client    *egress.Client
	extractor declarative.Extractor
	host      *wasmhost.Host

	mu      sync.Mutex
	modules map[string]*wasmhost.Plugin // by manifest path

	// Process plugins run under one bounded supervisor per worker process,
	// created on first use unless SetProcessSupervisor installed one.
	procMu   sync.Mutex
	proc     *process.Supervisor
	procOpts process.Options
	procErr  error
}

// NewRunner builds a Runner. extractor may be nil, in which case scrapers fail
// with declarative.ErrNoExtractor.
func NewRunner(client *egress.Client, extractor declarative.Extractor, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		client:    client,
		extractor: extractor,
		host:      wasmhost.New(wasmhost.Options{Client: client, Logger: log}),
		modules:   map[string]*wasmhost.Plugin{},
		procOpts:  process.Options{Client: client, Logger: log},
	}
}

// SetProcessSupervisor installs the supervisor used for runtime "process"
// plugins (tests use it to bound capacity and point at the SDK). The Runner
// takes ownership and closes it.
func (r *Runner) SetProcessSupervisor(s *process.Supervisor) {
	r.procMu.Lock()
	defer r.procMu.Unlock()
	r.proc, r.procErr = s, nil
}

// ProcessSupervisor returns the process-plugin supervisor, creating it from
// the OPENGTM_PLUGIN_* environment on first use. Creating it also reclaims
// processes a crashed worker left behind (process.Sweep) and hardens the
// worker against same-user plugins reading its /proc entries, so the worker
// calls this at start-up when its catalog holds process plugins.
func (r *Runner) ProcessSupervisor() (*process.Supervisor, error) {
	r.procMu.Lock()
	defer r.procMu.Unlock()
	if r.proc != nil || r.procErr != nil {
		return r.proc, r.procErr
	}
	opts, err := process.OptionsFromEnv(r.procOpts, nil)
	if err == nil {
		r.proc, err = process.NewSupervisor(opts)
	}
	if err != nil {
		r.procErr = fmt.Errorf("%w: process plugin supervisor: %v", ErrUnsupported, err)
	}
	return r.proc, r.procErr
}

// Run executes p once. onProgress (optional) is called after each fetched
// scraper page.
func (r *Runner) Run(ctx context.Context, p *manifest.Plugin, inputs map[string]any,
	onProgress func(declarative.Progress)) (Outcome, error) {
	secrets := envSecrets{p}
	switch {
	case p.Runtime == "declarative" && p.Kind == "provider":
		res, err := declarative.RunProvider(ctx, p, inputs, secrets, r.client)
		if err != nil {
			return Outcome{}, err
		}
		out := Outcome{CostUSD: res.CostUSD}
		if !res.Success {
			out.ProviderError = res.Error
			return out, nil
		}
		out.Records = []Record{{
			Data: res.FieldsJSON,
			Evidence: map[string]any{
				"source":     res.Evidence,
				"confidence": res.Confidence,
				"cost_usd":   res.CostUSD,
			},
		}}
		return out, nil

	case p.Runtime == "declarative" && p.Kind == "scraper":
		if r.extractor == nil {
			return Outcome{}, declarative.ErrNoExtractor
		}
		res, err := declarative.RunScraper(ctx, p, inputs, declarative.ScraperDeps{
			Client: r.client, Extractor: r.extractor, Secrets: secrets, OnProgress: onProgress,
		})
		if err != nil {
			return Outcome{}, err
		}
		out := Outcome{Pages: res.Pages, Stopped: res.Stopped, Records: make([]Record, 0, len(res.Records))}
		for _, rec := range res.Records {
			data := make(map[string]any, len(rec.Fields))
			for k, v := range rec.Fields {
				data[k] = v
			}
			out.Records = append(out.Records, Record{Data: data, Evidence: rec.Evidence})
		}
		return out, nil

	case p.Runtime == "process":
		return r.runProcess(ctx, p, inputs, secrets, onProgress)

	case p.Runtime == "wasm":
		pl, err := r.module(ctx, p)
		if err != nil {
			return Outcome{}, err
		}
		raw, call, err := pl.Invoke(ctx, inputs, nil, secrets)
		if err != nil {
			return Outcome{}, err
		}
		return wasmOutcome(p, raw, call), nil
	}
	return Outcome{}, fmt.Errorf("%w: runtime %s, kind %s", ErrUnsupported, p.Runtime, p.Kind)
}

// runProcess runs a process plugin under the supervisor. Records come back
// with the host's own fetch evidence merged in; the plugin never writes tenant
// data, and the caller commits results under the job lease.
func (r *Runner) runProcess(ctx context.Context, p *manifest.Plugin, inputs map[string]any,
	secrets envSecrets, onProgress func(declarative.Progress)) (Outcome, error) {
	if !slices.Contains(processKinds, p.Kind) {
		return Outcome{}, fmt.Errorf("%w: process runtime for kind %s", ErrUnsupported, p.Kind)
	}
	sup, err := r.ProcessSupervisor()
	if err != nil {
		return Outcome{}, err
	}
	// Resolve only declared secrets; the supervisor delivers them over the
	// control socket and nowhere else.
	resolved := map[string]string{}
	for _, name := range p.Capabilities.Secrets {
		if v, err := secrets.Secret(ctx, name); err == nil && v != "" {
			resolved[name] = v
		}
	}
	req := process.Request{Inputs: inputs, Secrets: resolved}
	if onProgress != nil {
		req.OnProgress = func(pr process.Progress) {
			var d declarative.Progress
			if pr.Pages != nil {
				d.Pages = *pr.Pages
			}
			if pr.Records != nil {
				d.Records = *pr.Records
			}
			onProgress(d)
		}
	}
	res, err := sup.Run(ctx, p, req)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Pages: res.Pages, CostUSD: res.CostUSD, Stopped: res.Stopped, ProviderError: res.ProviderError,
		Records: make([]Record, 0, len(res.Records))}
	for _, rec := range res.Records {
		out.Records = append(out.Records, Record{Data: rec.Fields, Evidence: rec.Evidence})
	}
	return out, nil
}

// wasmOutcome maps a kind export's output (docs/plugins/wasm-abi.md) onto
// records. The host's own fetch evidence is attached to every record, since a
// module's self-reported evidence is informational only.
func wasmOutcome(p *manifest.Plugin, raw map[string]any, call *wasmhost.CallResult) Outcome {
	out := Outcome{Pages: len(call.Fetches)}
	evidence := func(plugin any) map[string]any {
		return map[string]any{"fetches": call.Fetches, "plugin": plugin}
	}
	if cost, ok := raw["cost_usd"].(float64); ok {
		out.CostUSD = cost
	}
	switch p.Kind {
	case "scraper":
		recs, _ := raw["records"].([]any)
		for _, r := range recs {
			m, _ := r.(map[string]any)
			fields, _ := m["fields"].(map[string]any)
			if fields == nil {
				continue
			}
			out.Records = append(out.Records, Record{Data: fields, Evidence: evidence(m["evidence"])})
		}
	case "provider":
		if e, _ := raw["error"].(string); e != "" {
			out.ProviderError = e
			return out
		}
		if fields, _ := raw["fields"].(map[string]any); len(fields) > 0 {
			out.Records = []Record{{Data: fields, Evidence: evidence(raw["evidence"])}}
		}
	case "function":
		out.Records = []Record{{Data: map[string]any{"result": raw["result"]}, Evidence: evidence(nil)}}
	case "tool":
		out.Records = []Record{{Data: map[string]any{"output": raw["output"]}, Evidence: evidence(nil)}}
	}
	return out
}

// module compiles a WebAssembly plugin once per process.
func (r *Runner) module(ctx context.Context, p *manifest.Plugin) (*wasmhost.Plugin, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pl, ok := r.modules[p.Path]; ok {
		return pl, nil
	}
	pl, err := r.host.Load(ctx, p)
	if err != nil {
		return nil, err
	}
	r.modules[p.Path] = pl
	return pl, nil
}

// Close releases compiled modules and stops the process supervisor, killing
// any plugin process still running.
func (r *Runner) Close(ctx context.Context) {
	r.mu.Lock()
	for path, pl := range r.modules {
		_ = pl.Close(ctx)
		delete(r.modules, path)
	}
	r.mu.Unlock()
	r.procMu.Lock()
	if r.proc != nil {
		r.proc.Close()
		r.proc = nil
	}
	r.procMu.Unlock()
}

// envSecrets resolves a plugin's declared secrets from the process
// environment, as the Python runtime falls back to today. Per-workspace
// secrets live in the legacy SQLite control plane until it moves to
// PostgreSQL (RFC milestone M8); undeclared names never resolve.
type envSecrets struct{ p *manifest.Plugin }

func (s envSecrets) Secret(_ context.Context, name string) (string, error) {
	if !s.p.HasSecret(name) {
		return "", nil
	}
	return os.Getenv(name), nil
}
