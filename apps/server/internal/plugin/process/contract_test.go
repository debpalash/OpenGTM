//go:build linux

package process

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The contract tests run the real Python SDK against the real Go supervisor
// and check every frame that crosses the socket, in both directions, against
// packages/contracts/plugin-process.v1.schema.json.

type contractSchemas struct{ plugin, host *jsonschema.Schema }

func loadContract(t testing.TB) contractSchemas {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "packages", "contracts", "plugin-process.v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("mem:///process.json", doc); err != nil {
		t.Fatal(err)
	}
	compile := func(def string) *jsonschema.Schema {
		s, err := c.Compile("mem:///process.json#/$defs/" + def)
		if err != nil {
			t.Fatalf("compile %s: %v", def, err)
		}
		return s
	}
	return contractSchemas{plugin: compile("PluginMessage"), host: compile("HostMessage")}
}

func (c contractSchemas) check(dir string, body []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return err
	}
	if dir == "in" {
		return c.plugin.Validate(doc)
	}
	return c.host.Validate(doc)
}

// recorder collects frames through Options.Trace.
type recorder struct {
	mu     sync.Mutex
	frames []struct{ dir, body string }
}

func (r *recorder) trace(dir string, frame []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, struct{ dir, body string }{dir, string(frame)})
}

func (r *recorder) types() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for _, f := range r.frames {
		var m struct{ Type string }
		_ = json.Unmarshal([]byte(f.body), &m)
		out[f.dir+":"+m.Type] = true
	}
	return out
}

func (r *recorder) validate(t testing.TB, c contractSchemas) int {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.frames {
		if err := c.check(f.dir, []byte(f.body)); err != nil {
			t.Errorf("frame (%s) violates the contract: %v\n%s", f.dir, err, f.body)
		}
	}
	return len(r.frames)
}

func sdkSpec(kind string) pspec {
	return pspec{
		name: "sdk_all_" + kind, kind: kind, script: "sdk_all.py", secrets: []string{"API_KEY"},
		network: []string{"https://api.example.com"},
	}
}

func TestSchemaRejectsMalformedFrames(t *testing.T) {
	c := loadContract(t)
	bad := map[string]string{
		"fetch without id":          `{"type":"fetch","url":"https://x.example/"}`,
		"fetch with both bodies":    `{"type":"fetch","id":1,"url":"https://x.example/","body":"a","body_base64":"YQ=="}`,
		"fetch with bad method":     `{"type":"fetch","id":1,"url":"https://x.example/","method":"TRACE"}`,
		"unknown plugin message":    `{"type":"teleport"}`,
		"hello without sdk":         `{"type":"hello","protocols":[1]}`,
		"hello with no protocols":   `{"type":"hello","protocols":[],"sdk":{"name":"a","version":"1","language":"py"}}`,
		"result with negative cost": `{"type":"result","cost_usd":-1}`,
		"failure without code":      `{"type":"failure","message":"x"}`,
		"record without fields":     `{"type":"records","records":[{"evidence":{}}]}`,
		"progress fraction > 1":     `{"type":"progress","fraction":2}`,
		"log with bad level":        `{"type":"log","message":"x","level":"loud"}`,
	}
	for name, body := range bad {
		if err := c.check("in", []byte(body)); err == nil {
			t.Errorf("%s: plugin message accepted", name)
		}
	}
	badHost := map[string]string{
		"init without secrets":         `{"type":"init","protocol":1,"run_id":"r","plugin":{"name":"a","version":"1","kind":"provider"},"inputs":{},"limits":{"timeout_seconds":1,"max_pages":1,"max_response_bytes":1,"memory_mb":1,"max_frame_bytes":2048},"deadline_unix_ms":1}`,
		"fetch_result with both":       `{"type":"fetch_result","id":1,"status":200,"error":{"code":"x","message":"y"}}`,
		"fetch_result with neither":    `{"type":"fetch_result","id":1}`,
		"cancel with unknown reason":   `{"type":"cancel","reason":"because","grace_ms":1}`,
		"init string secret value int": `{"type":"init","protocol":1,"run_id":"r","plugin":{"name":"a","version":"1","kind":"provider"},"inputs":{},"secrets":{"A":1},"limits":{"timeout_seconds":1,"max_pages":1,"max_response_bytes":1,"memory_mb":1,"max_frame_bytes":2048},"deadline_unix_ms":1}`,
	}
	for name, body := range badHost {
		if err := c.check("out", []byte(body)); err == nil {
			t.Errorf("%s: host message accepted", name)
		}
	}
}

func TestSDKAndSupervisorSpeakTheContract(t *testing.T) {
	schemas := loadContract(t)
	rec := &recorder{}
	rt := okTransport()
	s := newSup(t, Options{Client: testClient(t, rt), Trace: rec.trace})
	secret := map[string]string{"API_KEY": "sk-contract-0001"}
	ctx := context.Background()
	prog := 0

	// provider: fetch, secret, log, progress, cost
	out, err := s.Run(ctx, sdkSpec("provider").build(t), Request{Inputs: map[string]any{"domain": "acme.example"}, Secrets: secret,
		OnProgress: func(Progress) { prog++ }})
	if err != nil {
		t.Fatal(err)
	}
	f := out.Records[0].Fields
	if f["status"] != float64(200) || f["domain"] != "acme.example" || out.CostUSD != 0.01 || prog != 1 {
		t.Fatalf("provider outcome: %+v (progress %d)", out, prog)
	}
	if asMap(t, f["body"])["hello"] != "/v1/companies" {
		t.Fatalf("body: %v", f["body"])
	}

	// provider with no answer, in both spellings
	for _, mode := range []string{"none", "noresult"} {
		out := mustRun(t, s, sdkSpec("provider").build(t), map[string]any{"domain": "x.example", "mode": mode}, secret)
		want := map[string]string{"none": "no_data", "noresult": "not_found"}[mode]
		if len(out.Records) != 0 || out.ProviderError != want {
			t.Fatalf("%s: %+v", mode, out)
		}
	}

	// scraper (async generator): records stream in batches and carry evidence
	out = mustRun(t, s, sdkSpec("scraper").build(t), map[string]any{"n": 450, "pad": 1000}, secret)
	if len(out.Records) != 450 || out.Stopped != "done" {
		t.Fatalf("scraper: %d records, stopped %q", len(out.Records), out.Stopped)
	}
	for i, r := range out.Records {
		if r.Fields["i"] != float64(i) {
			t.Fatalf("record %d: %v", i, r.Fields["i"])
		}
	}
	if pe := asMap(t, out.Records[0].Evidence["plugin"]); pe["source_url"] != "https://api.example.com/v1/page" || pe["index"] != float64(0) {
		t.Fatalf("plugin evidence: %v", pe)
	}
	if fe := out.Records[0].Evidence["fetches"].([]FetchEvidence); len(fe) != 1 {
		t.Fatalf("host evidence: %v", fe)
	}

	// function and tool wrap their return values like the wasm ABI does
	out = mustRun(t, s, sdkSpec("function").build(t), map[string]any{"x": 21}, nil)
	if out.Records[0].Fields["result"] != float64(42) {
		t.Fatalf("function: %v", out.Records[0].Fields)
	}
	out = mustRun(t, s, sdkSpec("tool").build(t), map[string]any{"q": "ada"}, nil)
	if asMap(t, out.Records[0].Fields["output"])["answer"] != "ADA" {
		t.Fatalf("tool: %v", out.Records[0].Fields)
	}

	// structured failures
	for _, c := range []struct {
		mode      string
		code      string
		retryable bool
	}{
		{"retryable", CodeUpstream, true},
		{"boom", CodePluginException, false},
		{"badjson", CodePluginException, false},
		{"denied", "capability_denied", false},
	} {
		_, err := run(t, s, sdkSpec("provider").build(t), map[string]any{"domain": "x.example", "mode": c.mode}, secret)
		wantError(t, err, c.code, c.retryable)
	}
	_, err = run(t, s, sdkSpec("provider").build(t), map[string]any{"domain": "x.example", "mode": "boom"}, secret)
	pe := err.(*Error)
	if !strings.Contains(pe.Message, "ValueError: kaput") || !strings.Contains(fmt.Sprint(pe.Details["traceback"]), "sdk_all.py") {
		t.Fatalf("exception details: %+v", pe)
	}
	// an input the manifest does not need is simply not passed; a missing
	// required one is a permanent invalid_input
	_, err = run(t, s, sdkSpec("function").build(t), map[string]any{}, nil)
	wantError(t, err, CodeInvalidInput, false)
	// a declared secret that the workspace has not configured
	_, err = run(t, s, sdkSpec("provider").build(t), map[string]any{"domain": "x.example"}, nil)
	wantError(t, err, CodeAuthFailed, false)

	n := rec.validate(t, schemas)
	seen := rec.types()
	for _, want := range []string{"in:hello", "out:init", "in:fetch", "out:fetch_result", "in:progress", "in:log", "in:records", "in:result", "in:failure"} {
		if !seen[want] {
			t.Errorf("no %s frame was exercised; seen: %v", want, keys(seen))
		}
	}
	t.Logf("validated %d frames against the contract", n)
}

func TestSDKHandlesCancelInSyncAndAsyncHandlers(t *testing.T) {
	schemas := loadContract(t)
	rec := &recorder{}
	s := newSup(t, Options{Client: testClient(t, okTransport()), Trace: rec.trace, CancelGrace: 5 * time.Second})
	secret := map[string]string{"API_KEY": "sk-cancel-0001"}

	for _, c := range []struct {
		name   string
		kind   string
		inputs map[string]any
	}{
		{"sync handler blocked in time.sleep", "provider", map[string]any{"domain": "x", "mode": "sleep"}},
		{"sync pure-Python busy loop", "provider", map[string]any{"domain": "x", "mode": "spin"}},
		{"async handler awaiting", "scraper", map[string]any{"mode": "asleep"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &captureLog{}
			s.log = log
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				for !strings.Contains(log.String(), "ready") {
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}()
			start := time.Now()
			_, err := s.Run(ctx, sdkSpec(c.kind).build(t), Request{Inputs: c.inputs, Secrets: secret})
			if err == nil || ctx.Err() == nil {
				t.Fatalf("err: %v", err)
			}
			// The SDK reacts to the cancel frame itself (SIGUSR1 into the main
			// thread, or task cancellation), so the plugin stops within a
			// fraction of the 5s grace period: the host never needed SIGKILL.
			limit := 3 * time.Second
			if d := time.Since(start); d > limit {
				t.Fatalf("cancel took %s", d)
			}
		})
	}
	if !rec.types()["out:cancel"] {
		t.Fatal("no cancel frame sent")
	}
	rec.validate(t, schemas)
}

// TestSDKToleratesFutureHostMessages drives the real SDK from a hand-written
// host: unknown fields in init and an unknown message type mid-run must be
// ignored, and the SDK must still honour a cancel that follows.
func TestSDKToleratesFutureHostMessages(t *testing.T) {
	requirePython(t)
	conn, child, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	cmd, err := ExecLauncher{}.Command(LaunchSpec{
		Argv: []string{py, filepath.Join(testdataDir(), "sdk_all.py")}, PluginDir: testdataDir(),
		RunDir: t.TempDir(), ControlFile: child,
		Env: []string{"PATH=/usr/bin:/bin", "PYTHONPATH=" + sdkPath(), "OPENGTM_PLUGIN_FD=3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = child.Close()
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	br := bufio.NewReader(conn)
	next := func() map[string]any {
		body, err := ReadFrame(br, 0)
		if err != nil {
			t.Fatalf("read: %v\n%s", err, stderr.String())
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	send := func(m map[string]any) {
		if err := WriteFrame(conn, m, 0); err != nil {
			t.Fatal(err)
		}
	}
	hello := next()
	if hello["type"] != "hello" || fmt.Sprint(hello["protocols"]) != "[1]" {
		t.Fatalf("first frame: %v", hello)
	}
	send(map[string]any{
		"type": "init", "protocol": 1, "run_id": "r", "future_field": true,
		"plugin": map[string]any{"name": "p", "version": "1", "kind": "provider", "extra": 1},
		"inputs": map[string]any{"domain": "x", "mode": "spin", "unused_input": 1}, "secrets": map[string]any{},
		"limits": map[string]any{"timeout_seconds": 5, "max_pages": 1, "max_response_bytes": 1000, "memory_mb": 64,
			"max_frame_bytes": 1 << 20, "new_limit": 3},
		"deadline_unix_ms": time.Now().Add(5 * time.Second).UnixMilli(),
	})
	for {
		if m := next(); m["type"] == "log" && m["message"] == "ready" {
			break
		}
	}
	send(map[string]any{"type": "from_the_future", "payload": []any{1, 2}})
	send(map[string]any{"type": "cancel", "reason": "cancelled", "grace_ms": 1000, "future_field": 1})
	got := next()
	if got["type"] != "failure" || got["code"] != "cancelled" {
		t.Fatalf("SDK reply: %v\n%s", got, stderr.String())
	}
}

// The thirty-line client printed at the end of docs/plugins/process-abi.md is
// run as a real plugin, so the documentation cannot drift from the protocol.
func TestTheDocumentedMinimalClientWorks(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot(), "docs", "plugins", "process-abi.md"))
	if err != nil {
		t.Fatal(err)
	}
	const fence = "```python\n"
	i := strings.LastIndex(string(doc), fence)
	if i < 0 {
		t.Fatal("no python block in process-abi.md")
	}
	code := string(doc)[i+len(fence):]
	code = code[:strings.Index(code, "```")]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newSup(t, Options{})
	out := mustRun(t, s, pspec{dir: dir, command: "[python3, main.py]"}.build(t), map[string]any{"mode": "hello"}, nil)
	if got := asMap(t, fieldsOf(t, out)["echo"]); got["mode"] != "hello" {
		t.Fatalf("echo: %v", got)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
