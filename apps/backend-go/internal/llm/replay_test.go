package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// vectors.json is recorded from the Python code by testdata/record.py.
type vectorFile struct {
	Complete        []complVec   `json:"complete"`
	AnthropicCall   []complVec   `json:"anthropic_call"`
	ExtractJSON     []complVec   `json:"extract_json"`
	ParseJSON       []parseVec   `json:"parse_json"`
	DefaultProvider []defaultVec `json:"default_provider"`
	Stream          []streamVec  `json:"stream"`
	ChatChain       []chatVec    `json:"chat_chain"`
	Cost            []costVec    `json:"cost"`
	ToolCall        []toolVec    `json:"tool_call"`
	Batch           []batchVec   `json:"batch"`
}

type recordedReq struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type scriptResp struct {
	Status  int               `json:"status"`
	JSON    json.RawMessage   `json:"json"`
	Text    *string           `json:"text"`
	CT      string            `json:"ct"`
	SSE     []string          `json:"sse"`
	Headers map[string]string `json:"headers"`
	Delay   float64           `json:"delay"`
}

type usageRow struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	RateLimit        int    `json:"rate_limit"`
	RateRemaining    int    `json:"rate_remaining"`
	RateReset        string `json:"rate_reset"`
}

type complCall struct {
	Kind        string   `json:"kind"`
	Prompt      string   `json:"prompt"`
	System      string   `json:"system"`
	MaxTokens   int      `json:"max_tokens"`
	Temperature *float64 `json:"temperature"`
}

type complVec struct {
	Name     string                  `json:"name"`
	Settings map[string]string       `json:"settings"`
	Script   map[string][]scriptResp `json:"script"`
	Env      map[string]string       `json:"env"`
	Calls    []complCall             `json:"calls"`
	Results  []json.RawMessage       `json:"results"`
	Requests []recordedReq           `json:"requests"`
	Sleeps   []float64               `json:"sleeps"`
	Errors   []string                `json:"usage_errors_all"`
	Usage    struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
		CacheReadTokens  int `json:"cache_read_tokens"`
		Calls            int `json:"calls"`
		Provider         string
	} `json:"usage"`
	UsageRows []usageRow `json:"usage_rows"`
}

type parseVec struct {
	Input     string          `json:"input"`
	OK        bool            `json:"ok"`
	Output    json.RawMessage `json:"output"`
	NonFinite bool            `json:"nonfinite"`
}

type defaultVec struct {
	Name      string            `json:"name"`
	Settings  map[string]string `json:"settings"`
	Default   string            `json:"default"`
	Providers []struct {
		ID         string `json:"id"`
		BaseURL    string `json:"base_url"`
		Model      string `json:"model"`
		TokenParam string `json:"token_param"`
		Native     string `json:"native"`
	} `json:"providers"`
	Anthropic *string `json:"anthropic_provider"`
}

type chatProv struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	APIKey           string `json:"api_key"`
	BaseURL          string `json:"base_url"`
	Model            string `json:"model"`
	OpenAICompatible bool   `json:"openai_compatible"`
}

type chatVec struct {
	Name     string            `json:"name"`
	Settings map[string]string `json:"settings"`
	Chain    []chatProv        `json:"chain"`
	Active   chatProv          `json:"active"`
}

type streamVec struct {
	Name      string                  `json:"name"`
	Script    map[string][]scriptResp `json:"script"`
	Provider  chatProv                `json:"provider"`
	Fallbacks []chatProv              `json:"fallbacks"`
	Messages  json.RawMessage         `json:"messages"`
	Tools     json.RawMessage         `json:"tools"`
	Frames    []struct {
		Kind      string          `json:"kind"`
		Data      json.RawMessage `json:"data"`
		Provider  string          `json:"provider"`
		Fallbacks []string        `json:"fallbacks"`
	} `json:"frames"`
	Requests []recordedReq `json:"requests"`
	Timeout  *float64      `json:"timeout"`
}

type costVec struct {
	Model string    `json:"model"`
	Usage []int     `json:"usage"`
	USD   float64   `json:"usd"`
	Price []float64 `json:"price"`
}

type toolVec struct {
	Name     string                  `json:"name"`
	Settings map[string]string       `json:"settings"`
	Script   map[string][]scriptResp `json:"script"`
	Args     struct {
		Messages     []any `json:"messages"`
		System       any   `json:"system"`
		Tools        []any `json:"tools"`
		ToolChoice   any   `json:"tool_choice"`
		OutputFormat any   `json:"output_format"`
		MaxTokens    int   `json:"max_tokens"`
	} `json:"args"`
	Requests  []recordedReq `json:"requests"`
	UsageRows []usageRow    `json:"usage_rows"`
}

type batchVec struct {
	Name       string                  `json:"name"`
	Settings   map[string]string       `json:"settings"`
	Script     map[string][]scriptResp `json:"script"`
	RequestsIn []struct {
		CustomID  string `json:"custom_id"`
		Prompt    string `json:"prompt"`
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
	} `json:"requests_in"`
	KW struct {
		BatchID        string   `json:"batch_id"`
		KnownResultIDs []string `json:"known_result_ids"`
	} `json:"kw"`
	Result    map[string]string `json:"result"`
	Submitted []string          `json:"submitted"`
	Requests  []recordedReq     `json:"requests"`
	UsageRows []usageRow        `json:"usage_rows"`
}

func loadVectors(t testing.TB) *vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

// replayServer serves a recorded script: the i-th request to a path gets the
// i-th response (the last one repeats) and every request is logged.
type replayServer struct {
	*httptest.Server
	mu     sync.Mutex
	counts map[string]int
	log    []recordedReq
}

var keepHeaders = []string{"authorization", "content-type", "x-api-key", "anthropic-version"}

func newReplay(t testing.TB, script map[string][]scriptResp) *replayServer {
	t.Helper()
	rs := &replayServer{counts: map[string]int{}}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hdrs := map[string]string{}
		for _, k := range keepHeaders {
			if v := r.Header.Get(k); v != "" {
				hdrs[k] = v
			}
		}
		rr := recordedReq{Method: r.Method, Path: r.URL.Path, Headers: hdrs}
		if len(bytes.TrimSpace(body)) > 0 {
			rr.Body = body
		}
		rs.mu.Lock()
		i := rs.counts[r.URL.Path]
		rs.counts[r.URL.Path]++
		rs.log = append(rs.log, rr)
		rs.mu.Unlock()
		seq := script[r.URL.Path]
		if len(seq) == 0 {
			http.Error(w, `{"error":"no script"}`, 404)
			return
		}
		resp := seq[min(i, len(seq)-1)]
		if resp.Delay > 0 {
			select {
			case <-time.After(time.Duration(resp.Delay * float64(time.Second))):
			case <-r.Context().Done():
				return
			}
		}
		status := resp.Status
		if status == 0 {
			status = 200
		}
		var data []byte
		ct := "application/json"
		switch {
		case resp.SSE != nil:
			ct = "text/event-stream"
			data = []byte(strings.Join(resp.SSE, "\n") + "\n")
		case resp.Text != nil:
			data = []byte(*resp.Text)
			ct = resp.CT
		default:
			data = bytes.ReplaceAll(resp.JSON, []byte("{{BASE}}"), []byte(rs.URL))
		}
		w.Header().Set("Content-Type", ct)
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *replayServer) requests() []recordedReq {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]recordedReq(nil), rs.log...)
}

// mapSettings is a Settings over a map; empty means unset (_db_get semantics).
type mapSettings map[string]string

func (m mapSettings) Get(_ context.Context, key, def string) (string, error) {
	if v := m[key]; v != "" {
		return v, nil
	}
	return def, nil
}

func resolveBase(m map[string]string, base string) mapSettings {
	out := mapSettings{}
	for k, v := range m {
		out[k] = strings.ReplaceAll(v, "{{BASE}}", base)
	}
	return out
}

type usageSink struct {
	mu   sync.Mutex
	rows []usageRow
}

func (u *usageSink) Record(_ context.Context, ws string, r UsageRecord) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if ws != "ws-test" {
		panic("unexpected workspace " + ws)
	}
	u.rows = append(u.rows, usageRow{r.Provider, r.Model, r.PromptTokens, r.CompletionTokens, r.RateLimit, r.RateRemaining, r.RateReset})
	return nil
}

type sleepLog struct {
	mu sync.Mutex
	d  []float64
}

func (s *sleepLog) Sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, float64(d.Round(time.Millisecond))/float64(time.Second))
	s.mu.Unlock()
	return nil
}

// newVecClient builds a Client against base for a recorded settings map.
func newVecClient(t testing.TB, settingsMap map[string]string, base string, env map[string]string, sink *usageSink, sl *sleepLog) *Client {
	t.Helper()
	cfg, err := config.LLMFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{
		Settings: resolveBase(settingsMap, base), HTTP: http.DefaultClient, Usage: sink, WorkspaceID: "ws-test",
		Config: cfg, Sleep: sl.Sleep, Jitter: func() float64 { return 0 },
	})
}

func canon(t testing.TB, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(bytes.ReplaceAll(raw, []byte("{{BASE}}"), []byte("BASE")), &v); err != nil {
		t.Fatalf("bad JSON %s: %v", raw, err)
	}
	return v
}

// topKeys lists the top-level keys of a JSON object in document order.
func topKeys(t testing.TB, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		tok, _ := dec.Token()
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// compareRequests checks method, path, headers and body of every recorded
// request against what the server received. ordered also checks the top-level
// key order of the body (OpenAI-compatible bodies are sent in Python dict order).
func compareRequests(t *testing.T, want, got []recordedReq, ordered bool) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("request count: want %d got %d\nwant %+v\ngot %+v", len(want), len(got), want, got)
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Method != g.Method || w.Path != g.Path {
			t.Fatalf("req %d: want %s %s got %s %s", i, w.Method, w.Path, g.Method, g.Path)
		}
		for _, k := range keepHeaders {
			if w.Headers[k] != g.Headers[k] {
				t.Errorf("req %d header %s: want %q got %q", i, k, w.Headers[k], g.Headers[k])
			}
		}
		if len(w.Body) == 0 || string(w.Body) == "null" {
			if len(g.Body) != 0 {
				t.Errorf("req %d: unexpected body %s", i, g.Body)
			}
			continue
		}
		if !reflect.DeepEqual(canon(t, w.Body), canon(t, g.Body)) {
			t.Errorf("req %d body:\nwant %s\ngot  %s", i, w.Body, g.Body)
		}
		if ordered && !reflect.DeepEqual(topKeys(t, w.Body), topKeys(t, g.Body)) {
			t.Errorf("req %d body key order: want %v got %v", i, topKeys(t, w.Body), topKeys(t, g.Body))
		}
	}
}

func compareRows(t *testing.T, want, got []usageRow) {
	t.Helper()
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("usage rows:\nwant %+v\ngot  %+v", want, got)
	}
}
