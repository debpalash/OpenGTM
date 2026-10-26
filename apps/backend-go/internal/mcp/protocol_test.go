package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

type fixtureIndex struct {
	Profiles map[string]struct {
		Flags map[string]any `json:"flags"`
		Cases []string       `json:"cases"`
	} `json:"profiles"`
	Tokens map[string]struct {
		ID           string   `json:"id"`
		UserID       *int32   `json:"user_id"`
		WorkspaceID  string   `json:"workspace_id"`
		Capabilities []string `json:"capabilities"`
	} `json:"tokens"`
}

type rpcFixture struct {
	Profile  string          `json:"profile"`
	Token    string          `json:"token"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
}

const fixtureRoot = "../../testdata/mcp"

func loadIndex(t *testing.T) fixtureIndex {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx fixtureIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func loadFixture(t *testing.T, rel string) rpcFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	var f rpcFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// ctxFor builds the Ctx and write flag of a fixture's token and profile.
func (idx fixtureIndex) ctxFor(f rpcFixture) (Ctx, Config) {
	tok := idx.Tokens[f.Token]
	caps := map[string]bool{}
	for _, c := range tok.Capabilities {
		caps[c] = true
	}
	cfg := Config{}
	if v, _ := idx.Profiles[f.Profile].Flags["MCP_WRITE_ENABLED"].(bool); v {
		cfg.WriteEnabled = true
	}
	return Ctx{WorkspaceID: tok.WorkspaceID, Capabilities: caps, UserID: tok.UserID, TokenID: tok.ID}, cfg
}

// pyBytes renders the fixture JSON the way Python's json.dumps did.
func pyBytes(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	b, err := pyjson.Dumps(raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func handle(s *Server, c Ctx, raw string) (string, bool) {
	b, ok := s.Handle(context.Background(), c, []byte(raw))
	return string(b), ok
}

// rpcCases lists the rpc/ fixtures of every profile in recording order.
func rpcCases(idx fixtureIndex) []string {
	var out []string
	for _, p := range idx.Profiles {
		for _, c := range p.Cases {
			if strings.HasPrefix(c, "rpc/") {
				out = append(out, c)
			}
		}
	}
	return out
}

func TestCatalogMatchesPythonToolsListFixtures(t *testing.T) {
	if len(Specs) != 15 {
		t.Fatalf("Specs has %d entries, want 15", len(Specs))
	}
	idx := loadIndex(t)
	checked := 0
	for _, rel := range rpcCases(idx) {
		f := loadFixture(t, rel)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(f.Request, &req)
		if req.Method != "tools/list" {
			continue
		}
		c, cfg := idx.ctxFor(f)
		s := NewServer(Deps{Cfg: cfg}, Registry{})
		got, ok := handle(s, c, string(f.Request))
		if !ok || got != pyBytes(t, f.Response) {
			t.Errorf("%s: tools/list differs from Python\n got: %.300s\nwant: %.300s", rel, got, pyBytes(t, f.Response))
		}
		checked++
	}
	if checked < 8 {
		t.Fatalf("only %d tools/list fixtures checked", checked)
	}
	// every spec appears in the all-capabilities fixture, in Python order
	f := loadFixture(t, "rpc/writes__tools_list_all_flag_on.json")
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	_ = json.Unmarshal(f.Response, &resp)
	if len(resp.Result.Tools) != len(Specs) {
		t.Fatalf("all-caps fixture lists %d tools, Specs has %d", len(resp.Result.Tools), len(Specs))
	}
	for i, tl := range resp.Result.Tools {
		if Specs[i].Name != tl.Name {
			t.Errorf("spec %d is %s, Python has %s", i, Specs[i].Name, tl.Name)
		}
	}
}

func TestListHidesWriteToolsWhenFlagOff(t *testing.T) {
	c := Ctx{Capabilities: AllCaps}
	names := func(s *Server) []string {
		var out []string
		for _, v := range s.List(c) {
			o := v.(*pyjson.Obj)
			if _, has := o.Vals["capability"]; has {
				t.Error("capability key leaked into tools/list")
			}
			out = append(out, o.Vals["name"].(string))
		}
		return out
	}
	off := names(NewServer(Deps{}, Registry{}))
	on := names(NewServer(Deps{Cfg: Config{WriteEnabled: true}}, Registry{}))
	if len(off) >= len(on) {
		t.Fatalf("flag off lists %d, on lists %d", len(off), len(on))
	}
	for _, n := range off {
		for _, sp := range Specs {
			if sp.Name == n && WriteCaps[sp.Capability] {
				t.Errorf("write tool %s visible with flag off", n)
			}
		}
	}
	if len(on) != 15 {
		t.Fatalf("flag on lists %d tools, want 15", len(on))
	}
	if got := NewServer(Deps{Cfg: Config{WriteEnabled: true}}, Registry{}).List(Ctx{}); len(got) != 0 {
		t.Fatalf("no capabilities lists %d tools", len(got))
	}
}

func TestInitializeMatchesFixture(t *testing.T) {
	idx := loadIndex(t)
	for _, rel := range []string{"rpc/read__initialize.json", "rpc/read__initialize_string_id.json", "rpc/read__initialize_no_id.json"} {
		f := loadFixture(t, rel)
		c, cfg := idx.ctxFor(f)
		got, ok := handle(NewServer(Deps{Cfg: cfg}, Registry{}), c, string(f.Request))
		if !ok || got != pyBytes(t, f.Response) {
			t.Errorf("%s:\n got: %s\nwant: %s", rel, got, pyBytes(t, f.Response))
		}
	}
}

func TestPingAndUnknownMethodMatchFixtures(t *testing.T) {
	idx := loadIndex(t)
	for _, rel := range []string{"rpc/read__ping.json", "rpc/read__unknown_method.json", "rpc/read__empty_method.json"} {
		f := loadFixture(t, rel)
		c, cfg := idx.ctxFor(f)
		got, ok := handle(NewServer(Deps{Cfg: cfg}, Registry{}), c, string(f.Request))
		if !ok || got != pyBytes(t, f.Response) {
			t.Errorf("%s:\n got: %s\nwant: %s", rel, got, pyBytes(t, f.Response))
		}
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	s := NewServer(Deps{}, Registry{})
	for _, m := range []string{"notifications/initialized", "notifications/cancelled", "notifications/anything"} {
		for _, body := range []string{
			`{"jsonrpc":"2.0","method":"` + m + `"}`,
			`{"jsonrpc":"2.0","method":"` + m + `","params":null}`,
			`{"jsonrpc":"2.0","id":3,"method":"` + m + `","params":[1]}`,
		} {
			if resp, ok := handle(s, Ctx{}, body); ok || resp != "" {
				t.Errorf("%s answered %q", body, resp)
			}
		}
	}
}

func TestIDTypePreserved(t *testing.T) {
	s := NewServer(Deps{}, Registry{})
	cases := map[string]string{
		`7`:                       `7`,
		`"a"`:                     `"a"`,
		`null`:                    `null`,
		`1.5`:                     `1.5`,
		`12345678901234567890123`: `12345678901234567890123`,
		`-0`:                      `0`,
		`1.0`:                     `1.0`,
		`1E2`:                     `100.0`,
		`"é"`:                     `"\u00e9"`,
	}
	for in, want := range cases {
		got, ok := handle(s, Ctx{}, `{"jsonrpc":"2.0","id":`+in+`,"method":"ping"}`)
		if !ok || got != `{"jsonrpc": "2.0", "id": `+want+`, "result": {}}` {
			t.Errorf("id %s -> %s", in, got)
		}
	}
	// absent id echoes null
	got, _ := handle(s, Ctx{}, `{"method":"ping"}`)
	if got != `{"jsonrpc": "2.0", "id": null, "result": {}}` {
		t.Errorf("absent id -> %s", got)
	}
	// an invalid id type is an invalid request
	for _, bad := range []string{`{}`, `[]`, `true`} {
		got, _ := handle(s, Ctx{}, `{"id":`+bad+`,"method":"ping"}`)
		if !strings.Contains(got, `"code": -32600`) || !strings.Contains(got, `"id": null`) {
			t.Errorf("id %s -> %s", bad, got)
		}
	}
}

func TestParseErrorAndInvalidRequest(t *testing.T) {
	s := NewServer(Deps{}, Registry{})
	const parse = `{"jsonrpc": "2.0", "id": null, "error": {"code": -32700, "message": "Parse error"}}`
	const invalid = `{"jsonrpc": "2.0", "id": null, "error": {"code": -32600, "message": "Invalid Request"}}`
	for _, in := range []string{``, `{`, `not json`, `{"a":1}}`, "\xff\xfe", `{"method": "ping",}`, `[1,2`} {
		if got, ok := handle(s, Ctx{}, in); !ok || got != parse {
			t.Errorf("%q -> %s", in, got)
		}
	}
	deep := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
	for _, in := range []string{`[]`, `[{"method":"ping"}]`, `"x"`, `7`, `null`, `true`, ` 3`, deep,
		`{"method": 5}`, `{"method": null}`, `{"method":"ping","x":` + deep + `}`,
		`{"method":"ping","x":` + strings.Repeat("[", 150) + strings.Repeat("]", 150) + `,"y":` + strings.Repeat("[", 250) + strings.Repeat("]", 250) + `}`} {
		if got, ok := handle(s, Ctx{}, in); !ok || got != invalid {
			t.Errorf("%.40q -> %s", in, got)
		}
	}
	// the size cap
	big := `{"method":"ping","x":"` + strings.Repeat("a", MaxMessageBytes) + `"}`
	if got, ok := handle(s, Ctx{}, big); !ok || got != invalid {
		t.Errorf("oversized -> %.80s", got)
	}
	// deep strings with brackets are not nesting
	ok := `{"method":"ping","x":"` + strings.Repeat("[", 500) + `"}`
	if got, _ := handle(s, Ctx{}, ok); !strings.Contains(got, `"result"`) {
		t.Errorf("brackets in a string -> %s", got)
	}
}

func TestParamsNotObjectIsInvalidParams(t *testing.T) {
	s := NewServer(Deps{}, Registry{})
	const want = `{"jsonrpc": "2.0", "id": 4, "error": {"code": -32602, "message": "Invalid params"}}`
	for _, m := range []string{"initialize", "tools/list", "tools/call", "ping", "nope"} {
		for _, p := range []string{`null`, `[]`, `"x"`, `5`, `true`, `[{"name":"find_leads"}]`, `{"a":12345678901234567890123}`} {
			got, ok := handle(s, Ctx{}, `{"jsonrpc":"2.0","id":4,"method":"`+m+`","params":`+p+`}`)
			if !ok || got != want {
				t.Errorf("%s params %s -> %s", m, p, got)
			}
		}
	}
	// tools/call params with a wrong typed name or arguments
	for _, p := range []string{`{"name":5}`, `{"name":null}`, `{"name":"x","arguments":"str"}`, `{"name":"x","arguments":[1]}`, `{"name":"x","arguments":5}`} {
		got, _ := handle(s, Ctx{}, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":`+p+`}`)
		if got != want {
			t.Errorf("tools/call %s -> %s", p, got)
		}
	}
	// absent name, null or absent arguments are valid (Python defaults)
	for _, p := range []string{`{}`, `{"name":"zzz"}`, `{"name":"zzz","arguments":null}`} {
		got, _ := handle(s, Ctx{}, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":`+p+`}`)
		if !strings.Contains(got, "Unknown tool") {
			t.Errorf("tools/call %s -> %s", p, got)
		}
	}
	if got, _ := handle(s, Ctx{}, `{"jsonrpc":"2.0","id":4,"method":"tools/call"}`); !strings.Contains(got, "Unknown tool: ") {
		t.Errorf("tools/call without params -> %s", got)
	}
}

func TestToolCallEnvelopeMatchesFixture(t *testing.T) {
	idx := loadIndex(t)
	cases := []string{"sse/read__tools_call_unknown.json", "sse/read__tools_call_capability_denied.json"}
	for _, rel := range cases {
		var f struct {
			Token   string `json:"token"`
			Profile string `json:"profile"`
			Request struct {
				Body json.RawMessage `json:"body"`
			} `json:"request"`
			Response struct {
				Body json.RawMessage `json:"body"`
			} `json:"response"`
		}
		b, err := os.ReadFile(filepath.Join(fixtureRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		c, cfg := idx.ctxFor(rpcFixture{Profile: f.Profile, Token: f.Token})
		s := NewServer(Deps{Cfg: cfg}, Registry{})
		got, ok := handle(s, c, string(f.Request.Body))
		if !ok || got != pyBytes(t, f.Response.Body) {
			t.Errorf("%s:\n got: %s\nwant: %s", rel, got, pyBytes(t, f.Response.Body))
		}
	}
	// a read tool through the registry: the text is echoed inside the envelope
	reg := NewRegistry([]Tool{{Name: "get_pipeline_stats", Capability: CapLeadsRead,
		Handler: func(context.Context, Ctx, Args) (string, error) { return "{\n  \"total\": 1\n}", nil }}}, nil)
	s := NewServer(Deps{}, reg)
	got, _ := handle(s, Ctx{Capabilities: ReadCaps}, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"get_pipeline_stats","arguments":{}}}`)
	want := `{"jsonrpc": "2.0", "id": 9, "result": {"content": [{"type": "text", "text": "{\n  \"total\": 1\n}"}], "isError": false}}`
	if got != want {
		t.Errorf("got %s", got)
	}
}

func TestToolErrorsAreMasked(t *testing.T) {
	boom := func(err error) Tool {
		return Tool{Name: "get_pipeline_stats", Capability: CapLeadsRead,
			Handler: func(context.Context, Ctx, Args) (string, error) { return "", err }}
	}
	for err, want := range map[error]string{
		fmt.Errorf("pgx: password=hunter2 failed"):    `internal error`,
		&ArgError{Field: "limit", Want: "an integer"}: `limit must be an integer`,
		ErrUnavailable: `not available in this build`,
	} {
		s := NewServer(Deps{}, NewRegistry([]Tool{boom(err)}, nil))
		got, _ := handle(s, Ctx{Capabilities: ReadCaps}, `{"id":1,"method":"tools/call","params":{"name":"get_pipeline_stats"}}`)
		if !strings.Contains(got, `{\"error\": \"`+want+`\"}`) || strings.Contains(got, "hunter2") {
			t.Errorf("%v -> %s", err, got)
		}
	}
	// a panicking handler is contained
	s := NewServer(Deps{}, NewRegistry([]Tool{{Name: "get_pipeline_stats", Capability: CapLeadsRead,
		Handler: func(context.Context, Ctx, Args) (string, error) { panic("boom secret") }}}, nil))
	got, _ := handle(s, Ctx{Capabilities: ReadCaps}, `{"id":1,"method":"tools/call","params":{"name":"get_pipeline_stats"}}`)
	if !strings.Contains(got, `internal error`) || strings.Contains(got, "secret") {
		t.Errorf("panic -> %s", got)
	}
}

func TestHandlerNeverPanicsOnFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	reg := NewRegistry([]Tool{{Name: "get_pipeline_stats", Capability: CapLeadsRead,
		Handler: func(context.Context, Ctx, Args) (string, error) { return "{}", nil }}}, nil)
	s := NewServer(Deps{}, reg)
	c := Ctx{Capabilities: AllCaps}
	vals := []string{`null`, `true`, `1`, `-1.5e3`, `"x"`, `"\ud800"`, `[]`, `{}`, `[1,"a",null]`, `{"name":"get_pipeline_stats"}`,
		`{"name":["x"],"arguments":{}}`, `99999999999999999999999`, `"\u00e9\u4e2d"`}
	methods := []string{`"initialize"`, `"ping"`, `"tools/list"`, `"tools/call"`, `"notifications/x"`, `"nope"`, `5`, `null`, `""`}
	check := func(in []byte) {
		resp, ok := s.Handle(context.Background(), c, in)
		if ok && !json.Valid(resp) {
			t.Fatalf("invalid JSON answer %q for %q", resp, in)
		}
		if !ok && resp != nil {
			t.Fatalf("respond=false with bytes for %q", in)
		}
	}
	for i := 0; i < 100; i++ { // random bytes, some mutated valid messages
		b := make([]byte, rng.Intn(60))
		rng.Read(b)
		check(b)
		base := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_pipeline_stats","arguments":{"a":[1,2]}}}`)
		for j := 0; j < 1+rng.Intn(4); j++ {
			base[rng.Intn(len(base))] = byte(rng.Intn(256))
		}
		check(base)
	}
	for i := 0; i < 100; i++ { // structurally valid, randomly typed
		body := fmt.Sprintf(`{"jsonrpc":%s,"id":%s,"method":%s,"params":%s}`,
			vals[rng.Intn(len(vals))], vals[rng.Intn(len(vals))], methods[rng.Intn(len(methods))], vals[rng.Intn(len(vals))])
		check([]byte(body))
		body = fmt.Sprintf(`{"method":"tools/call","params":{"name":%s,"arguments":%s}}`, vals[rng.Intn(len(vals))], vals[rng.Intn(len(vals))])
		check([]byte(body))
	}
}

func TestToolTimeoutReturnsErrorText(t *testing.T) {
	var sawCancel = make(chan struct{})
	reg := NewRegistry([]Tool{{Name: "get_pipeline_stats", Capability: CapLeadsRead,
		Handler: func(ctx context.Context, _ Ctx, _ Args) (string, error) {
			<-ctx.Done()
			close(sawCancel)
			return "", ctx.Err()
		}}, {Name: "find_leads", Capability: CapLeadsRead,
		Handler: func(ctx context.Context, _ Ctx, _ Args) (string, error) {
			time.Sleep(2 * time.Second) // ignores its context
			return "{}", nil
		}}}, nil)
	s := NewServer(Deps{}, reg)
	s.Timeout = 50 * time.Millisecond
	for _, tool := range []string{"get_pipeline_stats", "find_leads"} {
		start := time.Now()
		got, ok := handle(s, Ctx{Capabilities: ReadCaps}, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		if !ok || time.Since(start) > time.Second {
			t.Fatalf("%s: took %v", tool, time.Since(start))
		}
		if !strings.Contains(got, `"isError": false`) || !strings.Contains(got, `{\"error\": \"tool timed out after 50ms\"}`) {
			t.Errorf("%s -> %s", tool, got)
		}
	}
	select {
	case <-sawCancel:
	case <-time.After(time.Second):
		t.Error("handler context was not cancelled")
	}
}
