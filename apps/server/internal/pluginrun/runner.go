package pluginrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/wasmhost"
)

// ErrUnsupported marks a plugin this host cannot run (for example the
// out-of-process runtime, which arrives with the Python specialists).
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
	}
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

// Close releases compiled modules.
func (r *Runner) Close(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for path, pl := range r.modules {
		_ = pl.Close(ctx)
		delete(r.modules, path)
	}
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
