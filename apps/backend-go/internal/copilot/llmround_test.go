package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

type mapSettings map[string]string

func (m mapSettings) Get(_ context.Context, k, def string) (string, error) {
	if v, ok := m[k]; ok && v != "" {
		return v, nil
	}
	return def, nil
}

type scriptedResp struct {
	Status int      `json:"status"`
	Text   string   `json:"text"`
	SSE    []string `json:"sse"`
}

type fakeProvider struct {
	mu     sync.Mutex
	script map[string][]scriptedResp
	counts map[string]int
	log    []recordedReq
}

type recordedReq struct {
	Path          string          `json:"path"`
	Authorization string          `json:"authorization"`
	Body          json.RawMessage `json:"body"`
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.log = append(f.log, recordedReq{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: b})
	i := f.counts[r.URL.Path]
	f.counts[r.URL.Path] = i + 1
	seq := f.script[r.URL.Path]
	f.mu.Unlock()
	if len(seq) == 0 {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("no script"))
		return
	}
	resp := seq[min(i, len(seq)-1)]
	if resp.Status == 0 {
		resp.Status = 200
	}
	if resp.SSE != nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(resp.Status)
		for _, l := range resp.SSE {
			_, _ = w.Write([]byte(l + "\n"))
		}
		return
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write([]byte(resp.Text))
}

func newTestLLM() *llm.Client {
	return llm.New(llm.Options{Settings: mapSettings{}, HTTP: &http.Client{}})
}

// collect drains a native round into worker-frame JSON objects ({"t", "d", "cursor", "provider_done"}).
func collect(t *testing.T, src frameSource) []map[string]any {
	t.Helper()
	defer src.Close()
	var out []map[string]any
	for {
		f, err := src.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]any{"t": f.T}
		if len(f.D) > 0 {
			var d any
			if err := json.Unmarshal(f.D, &d); err != nil {
				t.Fatal(err)
			}
			m["d"] = d
		}
		if f.Cursor != nil {
			m["cursor"] = float64(*f.Cursor)
		}
		if f.ProviderDone != nil {
			m["provider_done"] = *f.ProviderDone
		}
		out = append(out, m)
	}
}

func TestNativeRoundMatchesPythonLLMRPC(t *testing.T) {
	var f struct {
		Cases []struct {
			Name  string `json:"name"`
			Chain []struct {
				ID, Name, Model string
				APIKey          string `json:"api_key"`
				BaseURL         string `json:"base_url"`
			} `json:"chain"`
			Body struct {
				Messages     json.RawMessage `json:"messages"`
				ToolsEnabled *bool           `json:"tools_enabled"`
				Cursor       int             `json:"provider_cursor"`
				BudgetHit    bool            `json:"round_budget_hit"`
			} `json:"body"`
			Script   map[string][]scriptedResp `json:"script"`
			Frames   []map[string]any          `json:"frames"`
			Requests []recordedReq             `json:"requests"`
		} `json:"cases"`
	}
	readVectors(t, "native_llm.json", &f)
	catalog, _ := json.Marshal(Schemas())
	var catalogV any
	_ = json.Unmarshal(catalog, &catalogV)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			fp := &fakeProvider{script: c.Script, counts: map[string]int{}}
			srv := httptest.NewServer(fp)
			defer srv.Close()
			var chain []llm.ChatProvider
			for _, p := range c.Chain {
				base := strings.NewReplacer("$BASE", srv.URL, "$DEAD", "http://127.0.0.1:1").Replace(p.BaseURL)
				chain = append(chain, llm.ChatProvider{ID: p.ID, Name: p.Name, APIKey: p.APIKey, BaseURL: base, Model: p.Model, OpenAICompatible: true})
			}
			mv, err := canonjson.Decode(c.Body.Messages)
			if err != nil {
				t.Fatal(err)
			}
			var msgs []map[string]any
			for _, m := range mv.([]any) {
				if mm, ok := m.(map[string]any); ok { // a non-object entry is dropped by both sides
					msgs = append(msgs, mm)
				}
			}
			tools := c.Body.ToolsEnabled == nil || *c.Body.ToolsEnabled
			tn := &Turn{Deps: Deps{Engine: &Engine{LLM: newTestLLM(), Chain: func(context.Context) []llm.ChatProvider { return chain }}}}
			got := collect(t, tn.nativeRound(context.Background(), roundRequest{messages: msgs, cursor: c.Body.Cursor, toolsEnabled: tools, budgetHit: c.Body.BudgetHit}))
			if !reflect.DeepEqual(got, c.Frames) && !(len(got) == 0 && len(c.Frames) == 0) {
				t.Errorf("frames:\n got %v\nwant %v", got, c.Frames)
			}
			if len(fp.log) != len(c.Requests) {
				t.Fatalf("requests = %d, want %d", len(fp.log), len(c.Requests))
			}
			for i, want := range c.Requests {
				g := fp.log[i]
				if g.Path != want.Path || g.Authorization != want.Authorization {
					t.Errorf("request %d: %s %q, want %s %q", i, g.Path, g.Authorization, want.Path, want.Authorization)
				}
				var gb, wb map[string]any
				_ = json.Unmarshal(g.Body, &gb)
				_ = json.Unmarshal(want.Body, &wb)
				if wb["tools"] == "$CATALOG" {
					wb["tools"] = catalogV
				}
				if !reflect.DeepEqual(gb, wb) {
					t.Errorf("request %d body:\n got %s\nwant %s", i, g.Body, want.Body)
				}
			}
		})
	}
}
