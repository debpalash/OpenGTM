package kernels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures live with the Rust crate so both test suites share them.
var crateDir = filepath.Join("..", "..", "..", "..", "crates", "opengtm-kernels")

var (
	sharedOnce sync.Once
	shared     *Kernels
	sharedErr  error
)

// kernels returns one pool for the whole test binary; compiling the module is
// the expensive part.
func kernels(tb testing.TB) *Kernels {
	tb.Helper()
	sharedOnce.Do(func() { shared, sharedErr = Load(context.Background()) })
	if sharedErr != nil {
		tb.Fatalf("Load: %v", sharedErr)
	}
	return shared
}

func readFile(tb testing.TB, parts ...string) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{crateDir}, parts...)...))
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

type parityCase struct {
	Input         string          `json:"input"`
	DefaultRegion string          `json:"default_region"`
	Expected      json.RawMessage `json:"expected"`
}

type parityFixtures struct {
	Domain     []parityCase `json:"domain"`
	Email      []parityCase `json:"email"`
	Phone      []parityCase `json:"phone"`
	PersonName []parityCase `json:"person_name"`
}

func loadParity(tb testing.TB) parityFixtures {
	var f parityFixtures
	if err := json.Unmarshal(readFile(tb, "parity", "fixtures.json"), &f); err != nil {
		tb.Fatal(err)
	}
	return f
}

func expectedString(tb testing.TB, raw json.RawMessage) *string {
	tb.Helper()
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		tb.Fatal(err)
	}
	return s
}

func show(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *s)
}

func TestParitySingle(t *testing.T) {
	k := kernels(t)
	ctx := context.Background()
	f := loadParity(t)
	single := map[string]struct {
		cases []parityCase
		call  func(c parityCase) (string, bool, error)
	}{
		"domain": {f.Domain, func(c parityCase) (string, bool, error) { return k.NormalizeDomain(ctx, c.Input) }},
		"email":  {f.Email, func(c parityCase) (string, bool, error) { return k.NormalizeEmail(ctx, c.Input) }},
		"phone": {f.Phone, func(c parityCase) (string, bool, error) {
			return k.NormalizePhone(ctx, c.Input, c.DefaultRegion)
		}},
	}
	for kind, tc := range single {
		t.Run(kind, func(t *testing.T) {
			if len(tc.cases) < 100 {
				t.Fatalf("only %d cases", len(tc.cases))
			}
			for _, c := range tc.cases {
				got, ok, err := tc.call(c)
				if err != nil {
					t.Fatalf("%s(%q): %v", kind, c.Input, err)
				}
				want := expectedString(t, c.Expected)
				var gotp *string
				if ok {
					gotp = &got
				}
				if show(gotp) != show(want) {
					t.Errorf("%s(%q) = %s, python = %s", kind, c.Input, show(gotp), show(want))
				}
			}
		})
	}
	t.Run("person_name", func(t *testing.T) {
		for _, c := range f.PersonName {
			got, err := k.NormalizePersonName(ctx, c.Input)
			if err != nil {
				t.Fatal(err)
			}
			var want PersonName
			if err := json.Unmarshal(c.Expected, &want); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("person_name(%q) = %+v, python = %+v", c.Input, got, want)
			}
		}
	})
}

func TestParityBatch(t *testing.T) {
	k := kernels(t)
	ctx := context.Background()
	f := loadParity(t)
	for kind, cases := range map[string][]parityCase{"domain": f.Domain, "email": f.Email, "phone": f.Phone} {
		inputs := make([]string, len(cases))
		for i, c := range cases {
			inputs[i] = c.Input
		}
		got, err := k.NormalizeBatch(ctx, kind, inputs)
		if err != nil {
			t.Fatalf("%s batch: %v", kind, err)
		}
		for i, c := range cases {
			if want := expectedString(t, c.Expected); show(got[i]) != show(want) {
				t.Errorf("%s batch(%q) = %s, python = %s", kind, c.Input, show(got[i]), show(want))
			}
		}
	}
	inputs := make([]string, len(f.PersonName))
	for i, c := range f.PersonName {
		inputs[i] = c.Input
	}
	names, err := k.NormalizePersonNames(ctx, inputs)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range f.PersonName {
		var want PersonName
		if err := json.Unmarshal(c.Expected, &want); err != nil {
			t.Fatal(err)
		}
		if names[i] != want {
			t.Errorf("person_name batch(%q) = %+v, python = %+v", c.Input, names[i], want)
		}
	}
	if _, err := k.NormalizeBatch(ctx, "person_name", inputs); err == nil {
		t.Error("NormalizeBatch(person_name) should point callers to NormalizePersonNames")
	}
	if _, err := k.NormalizeBatch(ctx, "zip", nil); !isCode(err, "invalid_request") {
		t.Errorf("unknown kind: %v", err)
	}
	if got, err := k.NormalizeBatch(ctx, "email", nil); err != nil || len(got) != 0 {
		t.Errorf("empty batch = %v, %v", got, err)
	}
}

func isCode(err error, code string) bool {
	var kerr *Error
	return errors.As(err, &kerr) && kerr.Code == code
}

type extractCase struct {
	Name         string          `json:"name"`
	DocumentFile string          `json:"document_file"`
	Request      ExtractRequest  `json:"request"`
	Expected     *ExtractResult  `json:"expected"`
	ExpectedErr  *Error          `json:"expected_error"`
	Raw          json.RawMessage `json:"-"`
}

func loadExtractCases(tb testing.TB) []extractCase {
	var file struct {
		Cases []extractCase `json:"cases"`
	}
	if err := json.Unmarshal(readFile(tb, "testdata", "extract_cases.json"), &file); err != nil {
		tb.Fatal(err)
	}
	for i := range file.Cases {
		if name := file.Cases[i].DocumentFile; name != "" {
			file.Cases[i].Request.Document = string(readFile(tb, "testdata", name))
		}
	}
	return file.Cases
}

func TestExtractFixtures(t *testing.T) {
	k := kernels(t)
	cases := loadExtractCases(t)
	if len(cases) < 10 {
		t.Fatalf("only %d extract cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := k.Extract(context.Background(), c.Request)
			if c.ExpectedErr != nil {
				var kerr *Error
				if !errors.As(err, &kerr) {
					t.Fatalf("want *Error %q, got %v", c.ExpectedErr.Code, err)
				}
				if kerr.Code != c.ExpectedErr.Code || kerr.Field != c.ExpectedErr.Field || kerr.Message == "" {
					t.Fatalf("got %+v, want code %q field %q", kerr, c.ExpectedErr.Code, c.ExpectedErr.Field)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, *c.Expected) {
				g, _ := json.MarshalIndent(got, "", " ")
				w, _ := json.MarshalIndent(c.Expected, "", " ")
				t.Fatalf("got\n%s\nwant\n%s", g, w)
			}
		})
	}
}

func TestExtractNilFieldsAndEmptyResult(t *testing.T) {
	k := kernels(t)
	res, err := k.Extract(context.Background(), ExtractRequest{Document: "<p>x</p>", Items: "css:li"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records == nil || len(res.Records) != 0 {
		t.Fatalf("records = %#v, want empty non-nil", res.Records)
	}
}

func TestRequestTooLarge(t *testing.T) {
	k := kernels(t)
	_, err := k.Extract(context.Background(), ExtractRequest{Document: strings.Repeat("x", MaxRequestBytes+1)})
	if !isCode(err, "request_too_large") {
		t.Fatalf("got %v", err)
	}
	_, err = k.Extract(context.Background(), ExtractRequest{Document: strings.Repeat("x", 10<<20+1)})
	if !isCode(err, "document_too_large") {
		t.Fatalf("got %v", err)
	}
}

func TestLargeDocument(t *testing.T) {
	k := kernels(t)
	row := `<div class="r"><a href="/p/%d">Person %d</a><span class="t">Title %d</span></div>`
	var b strings.Builder
	for i := 0; b.Len() < 9<<20; i++ {
		fmt.Fprintf(&b, row, i, i, i)
	}
	res, err := k.Extract(context.Background(), ExtractRequest{
		Document: b.String(), BaseURL: "https://x.example/", Items: "css:div.r",
		Fields: map[string]string{"name": "css:a", "url": "css:a::attr(href)", "title": "css:.t"},
		Limit:  10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 10000 || res.Records[9999]["url"] != "https://x.example/p/9999" {
		t.Fatalf("got %d records, last %v", len(res.Records), res.Records[len(res.Records)-1])
	}
}

func TestMemoryLimitTrapsWithoutPoisoningPool(t *testing.T) {
	k, err := LoadWithOptions(context.Background(), Options{PoolSize: 1, MemoryLimitBytes: 24 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	doc := strings.Repeat("<div><span>x</span></div>", 300_000) // ~7.5 MB of HTML
	_, err = k.Extract(context.Background(), ExtractRequest{Document: doc, Items: "span", Fields: map[string]string{"t": "css:::text"}})
	var kerr *Error
	if err == nil || errors.As(err, &kerr) {
		t.Fatalf("expected the memory limit to trap the call, got %v", err)
	}
	t.Logf("trap: %v", err)
	if got, ok, err := k.NormalizeDomain(context.Background(), "https://www.acme.example/x"); err != nil || !ok || got != "acme.example" {
		t.Fatalf("pool unusable after trap: %q %v %v", got, ok, err)
	}
}

func TestConcurrency(t *testing.T) {
	k, err := LoadWithOptions(context.Background(), Options{PoolSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	team := loadExtractCases(t)[0]
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for i := range 20 {
				switch (g + i) % 4 {
				case 0:
					d, _, err := k.NormalizeDomain(ctx, fmt.Sprintf("https://www.Host%d.example/p", i))
					if err == nil && d != fmt.Sprintf("host%d.example", i) {
						err = fmt.Errorf("domain %q", d)
					}
					if err != nil {
						errs <- err
						return
					}
				case 1:
					v, err := k.NormalizeBatch(ctx, "email", []string{" A@B.io ", "x"})
					if err == nil && (v[0] == nil || *v[0] != "a@b.io" || v[1] != nil) {
						err = fmt.Errorf("batch %v", v)
					}
					if err != nil {
						errs <- err
						return
					}
				case 2:
					res, err := k.Extract(ctx, team.Request)
					if err == nil && !reflect.DeepEqual(res, *team.Expected) {
						err = errors.New("extract result differs under concurrency")
					}
					if err != nil {
						errs <- err
						return
					}
				case 3:
					_, err := k.Extract(ctx, ExtractRequest{Document: "<p>", Fields: map[string]string{"x": "css:p["}})
					if !isCode(err, "invalid_selector") {
						errs <- fmt.Errorf("want invalid_selector, got %v", err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCancellation(t *testing.T) {
	k, err := LoadWithOptions(context.Background(), Options{PoolSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := k.NormalizeDomain(ctx, "a.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: %v", err)
	}

	// Warm both instances so the timed call measures interruption of running
	// guest code, not instantiation.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = k.NormalizeDomain(context.Background(), "a.com") }()
	}
	wg.Wait()

	// A slow call: every one of 8000 nested items scans its whole subtree for
	// a selector that never matches (quadratic; about 5 s uncancelled on a
	// laptop without -race).
	doc := strings.Repeat("<div>", 8000) + "<span>x</span>" + strings.Repeat("</div>", 8000)
	req := ExtractRequest{Document: doc, Items: "div", Fields: map[string]string{"x": "css:div div div div div div p"}, Limit: 10000}
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = k.Extract(ctx, req)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v after %v", err, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
	// The interrupted instance is discarded; the pool keeps working.
	for range 4 {
		if d, ok, err := k.NormalizeDomain(context.Background(), "WWW.A.com"); err != nil || !ok || d != "a.com" {
			t.Fatalf("after cancel: %q %v %v", d, ok, err)
		}
	}
}

func TestPoolBlocksUntilContextDone(t *testing.T) {
	k, err := LoadWithOptions(context.Background(), Options{PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	inst, err := k.fast.acquire(context.Background()) // hold the only slot
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := k.NormalizeEmail(ctx, "a@b.c"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline while pool is exhausted, got %v", err)
	}
	k.fast.release(inst, false)
	if _, ok, err := k.NormalizeEmail(context.Background(), "a@b.c"); err != nil || !ok {
		t.Fatalf("after release: %v %v", ok, err)
	}
}

func TestClose(t *testing.T) {
	k, err := LoadWithOptions(context.Background(), Options{PoolSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.NormalizeDomain(context.Background(), "a.com"); err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := k.NormalizeDomain(context.Background(), "a.com"); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

func TestNoWASIImports(t *testing.T) {
	// The kernel must not depend on WASI (filesystem, clocks, randomness) and
	// may import only the Extism host environment.
	if strings.Contains(string(wasmModule), "wasi_snapshot_preview1") {
		t.Fatal("kernel module imports WASI")
	}
}
