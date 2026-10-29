package settings

import (
	"context"
	"testing"
)

func i64(v int64) *int64 { return &v }

func TestPerfDefaultsAndClamp(t *testing.T) {
	cases := []struct {
		name   string
		stored string // "" = unset
		want   int
	}{
		{"unset", "", 12},
		{"garbage", "abc", 12},
		{"float text", "3.5", 12},
		{"valid", "20", 20},
		{"zero is not clamped on read", "0", 0},
	}
	for _, c := range cases {
		st := newFake()
		if c.stored != "" {
			st.m["ENRICH_ROW_CONCURRENCY"] = c.stored
		}
		app, _ := testApp(t, st, nil)
		p := &Perf{App: app, Reg: mustLoad(t)}
		got, err := p.Get(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.RowConcurrency != c.want {
			t.Errorf("%s: row_concurrency %d, want %d", c.name, got.RowConcurrency, c.want)
		}
		if got.Defaults.RowConcurrency != 12 || got.Defaults.ProviderWorkers != 8 || got.Defaults.ProviderTimeout != 10 ||
			got.Defaults.MaxProviders != 8 || got.Defaults.RetryPasses != 1 {
			t.Errorf("%s: defaults %+v", c.name, got.Defaults)
		}
	}
}

func TestPerfPutClampsToBounds(t *testing.T) {
	st := newFake()
	app, _ := testApp(t, st, nil)
	p := &Perf{App: app, Reg: mustLoad(t)}
	got, err := p.Put(context.Background(), PerfUpdate{
		RowConcurrency: i64(999), ProviderWorkers: i64(0), ProviderTimeout: i64(2),
		MaxProviders: i64(-5), RetryPasses: i64(1 << 62),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ENRICH_ROW_CONCURRENCY": "64", "ENRICH_PROVIDER_WORKERS": "1", "ENRICH_PROVIDER_TIMEOUT": "3",
		"ENRICH_MAX_PROVIDERS": "0", "ENRICH_RETRY_PASSES": "5",
	}
	for k, v := range want {
		if st.m[k] != v {
			t.Errorf("%s stored %q, want %q", k, st.m[k], v)
		}
	}
	if got.RowConcurrency != 64 || got.ProviderWorkers != 1 || got.ProviderTimeout != 3 || got.MaxProviders != 0 || got.RetryPasses != 5 {
		t.Errorf("returned %+v", got)
	}
}

func TestPerfPutIgnoresNull(t *testing.T) {
	st := newFake()
	st.m["ENRICH_MAX_PROVIDERS"] = "4"
	app, _ := testApp(t, st, nil)
	p := &Perf{App: app, Reg: mustLoad(t)}
	got, err := p.Put(context.Background(), PerfUpdate{ProviderTimeout: i64(50)})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.m) != 2 || st.m["ENRICH_PROVIDER_TIMEOUT"] != "50" || st.m["ENRICH_MAX_PROVIDERS"] != "4" {
		t.Fatalf("store %v", st.m)
	}
	if got.MaxProviders != 4 || got.ProviderTimeout != 50 || got.RowConcurrency != 12 {
		t.Fatalf("returned %+v", got)
	}
}
