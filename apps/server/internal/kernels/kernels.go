package kernels

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/tetratelabs/wazero"
)

// wasmModule is built by crates/build-kernels.sh from crates/opengtm-kernels.
// It is an Extism plugin (exports normalize_*, extract) that also exports a
// direct-memory ABI (og_*), which this package uses because the Extism I/O
// protocol costs one host call per 8 bytes; see
// crates/opengtm-kernels/src/direct.rs and BENCHMARKS.md.
//
//go:embed opengtm_kernels.wasm
var wasmModule []byte

const (
	wasmPageBytes = 64 << 10

	// DefaultMemoryLimitBytes caps the linear memory of each instance. A
	// 10 MiB HTML document parses into a DOM several times its size.
	DefaultMemoryLimitBytes = 512 << 20

	// DefaultRecycleInputBytes retires an instance after a call whose request
	// exceeded this size. WebAssembly memory never shrinks, so this keeps idle
	// instances from pinning the peak memory of one large document.
	DefaultRecycleInputBytes = 1 << 20

	// MaxRequestBytes mirrors the kernel's own request limit
	// (crates/opengtm-kernels/src/api.rs). Larger requests are rejected
	// before they are copied into the sandbox.
	MaxRequestBytes = 2*(10<<20) + (1 << 20)
)

// compilationCache shares machine code between Load calls in one process, so
// a second Kernels (tests, a reload) skips the compile.
var compilationCache = wazero.NewCompilationCache()

// ErrClosed is returned by calls made after Close.
var ErrClosed = errors.New("kernels: closed")

// Error is a structured failure reported by a kernel, for example an invalid
// selector. Runtime failures (a trap such as hitting the memory limit, or
// cancellation) are reported as ordinary errors instead.
type Error struct {
	// Code is one of invalid_request, request_too_large, invalid_selector,
	// invalid_document, document_too_large or output_too_large.
	Code    string `json:"code"`
	Message string `json:"message"`
	// Field names the extract field the error belongs to, when there is one.
	Field string `json:"field,omitempty"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("kernels: %s (field %q): %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("kernels: %s: %s", e.Code, e.Message)
}

// Options tunes Load. Zero values select the defaults.
type Options struct {
	// PoolSize bounds concurrently running instances per pool (normalizers
	// and extraction have separate pools). Default GOMAXPROCS.
	PoolSize int
	// MemoryLimitBytes caps each instance's linear memory. Default 512 MiB.
	MemoryLimitBytes uint64
	// RecycleInputBytes: see DefaultRecycleInputBytes. Negative disables.
	RecycleInputBytes int
}

// Kernels runs the embedded kernel module in-process. It is safe for
// concurrent use.
//
// Normalizers run on a pool compiled without interruption checks: their cost
// is linear in the (bounded) input, so the context is honored before a call
// starts but a running call finishes. Extraction can be superlinear in
// document depth, so it runs on a pool whose calls are interrupted when the
// context is done, at the price of slower guest code.
type Kernels struct {
	fast    *pool
	extract *pool
	once    sync.Once
	err     error
}

// Load compiles the embedded module and returns Kernels with default options.
// Compilation takes a few hundred milliseconds; keep one Kernels per process.
func Load(ctx context.Context) (*Kernels, error) {
	return LoadWithOptions(ctx, Options{})
}

// LoadWithOptions is Load with explicit options.
func LoadWithOptions(ctx context.Context, opts Options) (*Kernels, error) {
	if opts.PoolSize <= 0 {
		opts.PoolSize = runtime.GOMAXPROCS(0)
	}
	if opts.MemoryLimitBytes == 0 {
		opts.MemoryLimitBytes = DefaultMemoryLimitBytes
	}
	if opts.RecycleInputBytes == 0 {
		opts.RecycleInputBytes = DefaultRecycleInputBytes
	}
	if pages := opts.MemoryLimitBytes / wasmPageBytes; pages < 1 || pages > 65536 {
		return nil, fmt.Errorf("kernels: memory limit %d bytes is out of range", opts.MemoryLimitBytes)
	}

	// The two pools compile different machine code; do it concurrently.
	var (
		wg               sync.WaitGroup
		fast, extract    *pool
		fastErr, extrErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); fast, fastErr = newPool(ctx, opts, false) }()
	go func() { defer wg.Done(); extract, extrErr = newPool(ctx, opts, true) }()
	wg.Wait()
	if err := errors.Join(fastErr, extrErr); err != nil {
		for _, p := range []*pool{fast, extract} {
			if p != nil {
				_ = p.close()
			}
		}
		return nil, err
	}
	return &Kernels{fast: fast, extract: extract}, nil
}

// Close waits for in-flight calls, then releases every instance and the
// compiled code. Calls made after Close return ErrClosed. Close is
// idempotent.
func (k *Kernels) Close() error {
	k.once.Do(func() { k.err = errors.Join(k.fast.close(), k.extract.close()) })
	return k.err
}

type valueRequest struct {
	Value         string `json:"value"`
	DefaultRegion string `json:"default_region,omitempty"`
}

type valueResponse struct {
	Value *string `json:"value"`
}

func (k *Kernels) normalizeValue(ctx context.Context, fn string, req valueRequest) (string, bool, error) {
	var resp valueResponse
	if err := k.fast.call(ctx, fn, req, &resp); err != nil {
		return "", false, err
	}
	if resp.Value == nil {
		return "", false, nil
	}
	return *resp.Value, true, nil
}

// NormalizeDomain returns the company domain key for a URL or host, matching
// apps/api/services/dedup.py normalize_domain. ok is false when there is none.
func (k *Kernels) NormalizeDomain(ctx context.Context, value string) (domain string, ok bool, err error) {
	return k.normalizeValue(ctx, "normalize_domain", valueRequest{Value: value})
}

// NormalizeEmail returns the trimmed, lowercased email identity key, matching
// apps/api/services/entities/people.py _email_key. ok is false when value is
// not address-shaped.
func (k *Kernels) NormalizeEmail(ctx context.Context, value string) (email string, ok bool, err error) {
	return k.normalizeValue(ctx, "normalize_email", valueRequest{Value: value})
}

// NormalizePhone returns the last ten decimal digits, matching
// apps/api/services/dedup.py normalize_phone. region is accepted for API
// stability and currently has no effect (the Python reference has none).
func (k *Kernels) NormalizePhone(ctx context.Context, value, region string) (phone string, ok bool, err error) {
	return k.normalizeValue(ctx, "normalize_phone", valueRequest{Value: value, DefaultRegion: region})
}

// NormalizePersonName splits a display name into full/first/last the way the
// workbook people source does (no honorific or suffix handling).
func (k *Kernels) NormalizePersonName(ctx context.Context, value string) (PersonName, error) {
	var name PersonName
	err := k.fast.call(ctx, "normalize_person_name", valueRequest{Value: value}, &name)
	return name, err
}

type batchRequest struct {
	Kind   string   `json:"kind"`
	Values []string `json:"values"`
}

// NormalizeBatch normalizes many values of one kind ("domain", "email" or
// "phone") in a single call. The result has one entry per input, nil where
// there is no value. Use NormalizePersonNames for names.
func (k *Kernels) NormalizeBatch(ctx context.Context, kind string, values []string) ([]*string, error) {
	if kind == "person_name" {
		return nil, &Error{Code: "invalid_request", Message: "use NormalizePersonNames for person_name"}
	}
	var resp struct {
		Values []*string `json:"values"`
	}
	if err := k.fast.call(ctx, "normalize_batch", batchRequest{Kind: kind, Values: values}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Values) != len(values) {
		return nil, fmt.Errorf("kernels: normalize_batch: got %d values for %d inputs", len(resp.Values), len(values))
	}
	return resp.Values, nil
}

// NormalizePersonNames is the batch form of NormalizePersonName.
func (k *Kernels) NormalizePersonNames(ctx context.Context, values []string) ([]PersonName, error) {
	var resp struct {
		Values []PersonName `json:"values"`
	}
	if err := k.fast.call(ctx, "normalize_batch", batchRequest{Kind: "person_name", Values: values}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Values) != len(values) {
		return nil, fmt.Errorf("kernels: normalize_batch: got %d values for %d inputs", len(resp.Values), len(values))
	}
	return resp.Values, nil
}

// Extract selects records and fields from one HTML or JSON document. Invalid
// selectors and oversized documents return an *Error. Selector cost can grow
// with document depth, so callers should pass a context with a deadline.
func (k *Kernels) Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error) {
	var res ExtractResult
	if err := k.extract.call(ctx, "extract", req, &res); err != nil {
		return ExtractResult{}, err
	}
	if res.Records == nil {
		res.Records = []map[string]string{}
	}
	return res, nil
}
