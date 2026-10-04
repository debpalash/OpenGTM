package kernels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// pool is one wazero runtime with the compiled kernel module and a bounded
// set of instances. Module instances are not goroutine-safe, so each call
// borrows one. Instances are created lazily and share the compiled code.
type pool struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	recycle  int

	tokens chan struct{} // one token per instance that may exist
	idle   chan *instance
	done   chan struct{} // closed by close
}

// instance is one sandboxed copy of the module with its exports resolved.
type instance struct {
	mod   api.Module
	alloc api.Function
	free  api.Function
	fns   map[string]api.Function
}

var exports = []string{
	"normalize_domain", "normalize_email", "normalize_phone",
	"normalize_person_name", "normalize_batch", "extract",
}

// runtimeConfig caps memory per module. interruptible compiles termination
// checks into the guest code so that context cancellation stops a running
// call; those checks make guest code several times slower (BENCHMARKS.md).
func runtimeConfig(memoryPages uint64, interruptible bool) wazero.RuntimeConfig {
	return wazero.NewRuntimeConfig().
		WithCompilationCache(compilationCache).
		WithCloseOnContextDone(interruptible).
		WithMemoryLimitPages(uint32(memoryPages))
}

func newPool(ctx context.Context, opts Options, interruptible bool) (*pool, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, runtimeConfig(opts.MemoryLimitBytes/wasmPageBytes, interruptible))
	p, err := initPool(ctx, rt, opts)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	return p, nil
}

func initPool(ctx context.Context, rt wazero.Runtime, opts Options) (*pool, error) {
	compiled, err := rt.CompileModule(ctx, wasmModule)
	if err != nil {
		return nil, fmt.Errorf("kernels: compile: %w", err)
	}
	// The module's only imports are the Extism host functions used by its
	// Extism exports. The direct ABI never calls them, so they are stubs: the
	// sandbox gets no WASI, filesystem, network, clock or randomness.
	env := rt.NewHostModuleBuilder("extism:host/env")
	for _, fn := range compiled.ImportedFunctions() {
		module, name, _ := fn.Import()
		if module != "extism:host/env" {
			return nil, fmt.Errorf("kernels: unexpected import %s.%s", module, name)
		}
		env.NewFunctionBuilder().
			WithGoModuleFunction(api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
				panic("kernels: Extism host function " + name + " is not available on the direct ABI")
			}), fn.ParamTypes(), fn.ResultTypes()).
			Export(name)
	}
	if _, err := env.Instantiate(ctx); err != nil {
		return nil, fmt.Errorf("kernels: host stubs: %w", err)
	}
	p := &pool{
		runtime:  rt,
		compiled: compiled,
		recycle:  opts.RecycleInputBytes,
		tokens:   make(chan struct{}, opts.PoolSize),
		idle:     make(chan *instance, opts.PoolSize),
		done:     make(chan struct{}),
	}
	for range opts.PoolSize {
		p.tokens <- struct{}{}
	}
	return p, nil
}

func (p *pool) newInstance(ctx context.Context) (*instance, error) {
	// Anonymous modules can be instantiated any number of times. The default
	// module config grants nothing: no args, env, stdio, FS or clocks.
	mod, err := p.runtime.InstantiateModule(ctx, p.compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, err
	}
	inst := &instance{
		mod:   mod,
		alloc: mod.ExportedFunction("og_alloc"),
		free:  mod.ExportedFunction("og_free"),
		fns:   make(map[string]api.Function, len(exports)),
	}
	for _, name := range exports {
		if f := mod.ExportedFunction("og_" + name); f != nil {
			inst.fns[name] = f
		}
	}
	if inst.alloc == nil || inst.free == nil || len(inst.fns) != len(exports) {
		_ = mod.Close(ctx)
		return nil, errors.New("module lacks the og_* direct ABI; rebuild it with crates/build-kernels.sh")
	}
	return inst, nil
}

// close waits for in-flight calls, then releases every instance and the
// runtime.
func (p *pool) close() error {
	close(p.done)
	for range cap(p.tokens) {
		<-p.tokens // wait for each in-flight call to give its token back
	}
	ctx := context.Background()
	var err error
	for {
		select {
		case inst := <-p.idle:
			err = errors.Join(err, inst.mod.Close(ctx))
			continue
		default:
		}
		return errors.Join(err, p.runtime.Close(ctx))
	}
}

func (p *pool) acquire(ctx context.Context) (*instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err // select below picks randomly among ready cases
	}
	select {
	case <-p.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.tokens:
	}
	select {
	case <-p.done: // close started while we waited; let it have the token.
		p.tokens <- struct{}{}
		return nil, ErrClosed
	default:
	}
	select {
	case inst := <-p.idle:
		return inst, nil
	default:
	}
	inst, err := p.newInstance(ctx)
	if err != nil {
		p.tokens <- struct{}{}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("kernels: instantiate: %w", err)
	}
	return inst, nil
}

// release returns inst to the pool, or closes it when it must not be reused.
func (p *pool) release(inst *instance, discard bool) {
	if discard {
		_ = inst.mod.Close(context.Background())
	} else {
		p.idle <- inst // never blocks: at most cap(tokens) instances exist
	}
	p.tokens <- struct{}{}
}

// invoke copies in into guest memory, runs og_<fn> and hands the response
// bytes to decode before freeing them. decode must not retain the slice.
func (inst *instance) invoke(ctx context.Context, fn string, in []byte, decode func([]byte) error) error {
	f := inst.fns[fn]
	if f == nil {
		return fmt.Errorf("unknown kernel function %q", fn)
	}
	res, err := inst.alloc.Call(ctx, uint64(len(in)))
	if err != nil {
		return err
	}
	ptr := uint32(res[0])
	if !inst.mod.Memory().Write(ptr, in) {
		return errors.New("request buffer out of range")
	}
	res, err = f.Call(ctx, uint64(ptr), uint64(len(in)))
	if err != nil {
		return err
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0])
	out, ok := inst.mod.Memory().Read(outPtr, outLen)
	if !ok {
		return errors.New("response buffer out of range")
	}
	decodeErr := decode(out) // out is a view of guest memory
	if _, err := inst.free.Call(ctx, uint64(outPtr), uint64(outLen)); err != nil {
		return errors.Join(decodeErr, err)
	}
	return decodeErr
}

// call runs one kernel with a JSON request and decodes its JSON response.
func (p *pool) call(ctx context.Context, fn string, req, resp any) error {
	in, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("kernels: %s: encode request: %w", fn, err)
	}
	if len(in) > MaxRequestBytes {
		return &Error{Code: "request_too_large", Message: fmt.Sprintf("request is %d bytes; the limit is %d", len(in), MaxRequestBytes)}
	}
	inst, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	var kernelErr *Error
	callErr := inst.invoke(ctx, fn, in, func(out []byte) error {
		// Responses are either {"error": {...}} or the result object, which
		// never has a top-level "error" key.
		if len(out) > 9 && string(out[:9]) == `{"error":` {
			var env struct {
				Error *Error `json:"error"`
			}
			if err := json.Unmarshal(out, &env); err != nil || env.Error == nil {
				return fmt.Errorf("malformed error response: %q", out)
			}
			kernelErr = env.Error
			return nil
		}
		return json.Unmarshal(out, resp)
	})
	// A failed call may leave the instance closed (cancellation) or in an
	// unknown state (trap), so it is never reused. Large calls are retired
	// because linear memory never shrinks.
	p.release(inst, callErr != nil || (p.recycle > 0 && len(in) > p.recycle))
	if callErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("kernels: %s: %w", fn, ctxErr)
		}
		return fmt.Errorf("kernels: %s: %w", fn, callErr)
	}
	if kernelErr != nil {
		return kernelErr
	}
	return nil
}
