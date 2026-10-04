package kernels

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	extism "github.com/extism/go-sdk"
)

// The embedded module is also a standard Extism plugin, so the plugin
// platform (and the extism CLI) can run it. These tests keep that path
// working and the benchmarks measure what the Extism I/O protocol costs
// compared with the direct ABI used by Kernels.

func extismPlugin(tb testing.TB) *extism.Plugin {
	tb.Helper()
	ctx := context.Background()
	manifest := extism.Manifest{Wasm: []extism.Wasm{extism.WasmData{Data: wasmModule, Name: "main"}}}
	compiled, err := extism.NewCompiledPlugin(ctx, manifest, extism.PluginConfig{
		RuntimeConfig: runtimeConfig(DefaultMemoryLimitBytes/wasmPageBytes, false),
	}, nil)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = compiled.Close(ctx) })
	p, err := compiled.Instance(ctx, extism.PluginInstanceConfig{})
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

func extismCall(tb testing.TB, p *extism.Plugin, fn string, req any) []byte {
	tb.Helper()
	in, err := json.Marshal(req)
	if err != nil {
		tb.Fatal(err)
	}
	rc, out, err := p.Call(fn, in)
	if err != nil || rc != 0 {
		tb.Fatalf("extism %s: rc=%d err=%v", fn, rc, err)
	}
	return out
}

func TestExtismProtocolMatchesDirectABI(t *testing.T) {
	p := extismPlugin(t)
	k := kernels(t)
	ctx := context.Background()
	for _, c := range loadExtractCases(t) {
		out := extismCall(t, p, "extract", c.Request)
		direct, err := k.Extract(ctx, c.Request)
		if c.ExpectedErr != nil {
			var env struct{ Error *Error }
			if json.Unmarshal(out, &env) != nil || env.Error == nil || env.Error.Code != c.ExpectedErr.Code || err == nil {
				t.Errorf("%s: extism %s, direct %v", c.Name, out, err)
			}
			continue
		}
		var res ExtractResult
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res, direct) || !reflect.DeepEqual(res, *c.Expected) {
			t.Errorf("%s: extism and direct results differ", c.Name)
		}
	}
	f := loadParity(t)
	for _, c := range f.Domain[:200] {
		var resp valueResponse
		if err := json.Unmarshal(extismCall(t, p, "normalize_domain", valueRequest{Value: c.Input}), &resp); err != nil {
			t.Fatal(err)
		}
		if show(resp.Value) != show(expectedString(t, c.Expected)) {
			t.Errorf("extism normalize_domain(%q) = %s", c.Input, show(resp.Value))
		}
	}
}

func BenchmarkExtismNormalizeDomain(b *testing.B) {
	p := extismPlugin(b)
	for b.Loop() {
		extismCall(b, p, "normalize_domain", valueRequest{Value: "https://www.Example.com/about?x=1"})
	}
}

func BenchmarkExtismNormalizeBatch1k(b *testing.B) {
	p := extismPlugin(b)
	req := batchRequest{Kind: "domain", Values: benchDomains(b)}
	for b.Loop() {
		extismCall(b, p, "normalize_batch", req)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(req.Values)), "ns/value")
}

func BenchmarkExtismExtractTeamPage(b *testing.B) {
	p := extismPlugin(b)
	req := benchTeamPage(b)
	b.SetBytes(int64(len(req.Document)))
	for b.Loop() {
		extismCall(b, p, "extract", req)
	}
}
