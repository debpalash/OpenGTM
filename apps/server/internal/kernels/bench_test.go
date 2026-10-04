package kernels

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tetratelabs/wazero"
)

// Microbenchmarks: in-process only, no network or database. See
// crates/opengtm-kernels/BENCHMARKS.md for results and the Python baseline.

// benchCorpus is testdata/bench_corpus.json: 1000 realistic values per kind,
// shared with parity/bench_python.py so both sides time the same inputs.
func benchCorpus(b *testing.B) map[string][]string {
	var corpus struct {
		Domain     []string `json:"domain"`
		Email      []string `json:"email"`
		Phone      []string `json:"phone"`
		PersonName []string `json:"person_name"`
	}
	if err := json.Unmarshal(readFile(b, "testdata", "bench_corpus.json"), &corpus); err != nil {
		b.Fatal(err)
	}
	return map[string][]string{
		"domain": corpus.Domain, "email": corpus.Email, "phone": corpus.Phone, "person_name": corpus.PersonName,
	}
}

func benchDomains(b *testing.B) []string { return benchCorpus(b)["domain"] }

func BenchmarkNormalizeDomain(b *testing.B) {
	k := kernels(b)
	ctx := context.Background()
	for b.Loop() {
		if _, _, err := k.NormalizeDomain(ctx, "https://www.Example.com/about?x=1"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNormalizeDomainParallel(b *testing.B) {
	k := kernels(b)
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			if _, _, err := k.NormalizeDomain(ctx, "https://www.Example.com/about?x=1"); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkNormalizeBatch1k(b *testing.B) {
	k := kernels(b)
	corpus := benchCorpus(b)
	for _, kind := range []string{"domain", "email", "phone", "person_name"} {
		b.Run(kind, func(b *testing.B) {
			ctx := context.Background()
			values := corpus[kind]
			for b.Loop() {
				var err error
				if kind == "person_name" {
					_, err = k.NormalizePersonNames(ctx, values)
				} else {
					_, err = k.NormalizeBatch(ctx, kind, values)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(values)), "ns/value")
		})
	}
}

// BenchmarkGoJSONOverhead isolates the host-side share of a batch call:
// encoding the request and decoding the response, with no WebAssembly.
func BenchmarkGoJSONOverhead(b *testing.B) {
	k := kernels(b)
	corpus := benchCorpus(b)
	ctx := context.Background()
	for _, kind := range []string{"domain", "email", "phone", "person_name"} {
		b.Run(kind, func(b *testing.B) {
			values := corpus[kind]
			var resp []byte
			var err error
			if kind == "person_name" {
				names, e := k.NormalizePersonNames(ctx, values)
				err = e
				resp, _ = json.Marshal(map[string]any{"values": names})
			} else {
				out, e := k.NormalizeBatch(ctx, kind, values)
				err = e
				resp, _ = json.Marshal(map[string]any{"values": out})
			}
			if err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				if _, err := json.Marshal(batchRequest{Kind: kind, Values: values}); err != nil {
					b.Fatal(err)
				}
				if kind == "person_name" {
					var r struct{ Values []PersonName }
					err = json.Unmarshal(resp, &r)
				} else {
					var r struct{ Values []*string }
					err = json.Unmarshal(resp, &r)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(values)), "ns/value")
		})
	}
}

func benchTeamPage(b *testing.B) ExtractRequest {
	return loadExtractCases(b)[0].Request
}

func BenchmarkExtractTeamPage(b *testing.B) {
	k := kernels(b)
	ctx := context.Background()
	req := benchTeamPage(b)
	b.SetBytes(int64(len(req.Document)))
	for b.Loop() {
		if _, err := k.Extract(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExtractTeamPageParallel(b *testing.B) {
	k := kernels(b)
	req := benchTeamPage(b)
	b.SetBytes(int64(len(req.Document)))
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			if _, err := k.Extract(ctx, req); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkInstantiate is the cost of a fresh instance (paid on first use of
// each pool slot and after recycling a large or failed call).
func BenchmarkInstantiate(b *testing.B) {
	k := kernels(b)
	ctx := context.Background()
	for b.Loop() {
		inst, err := k.fast.acquire(ctx)
		if err != nil {
			b.Fatal(err)
		}
		k.fast.release(inst, true)
	}
}

// BenchmarkLoadUncached is the one-time compile cost at process start.
func BenchmarkLoadUncached(b *testing.B) {
	saved := compilationCache
	defer func() { compilationCache = saved }()
	for b.Loop() {
		compilationCache = wazero.NewCompilationCache()
		k, err := Load(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		_ = k.Close()
	}
}
