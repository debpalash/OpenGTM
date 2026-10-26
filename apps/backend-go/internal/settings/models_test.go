package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

func newModels(t *testing.T, target string) *Models {
	t.Helper()
	var hosts []string
	if target != "" {
		u, _ := url.Parse(target)
		hosts = []string{u.Hostname()}
	}
	return &Models{Reg: mustLoad(t), HTTP: egress.NewClientForEnv("test", hosts), OpenRouterURL: target}
}

func TestModelsStaticListsMatchFixture(t *testing.T) {
	m := newModels(t, "")
	for _, c := range []struct{ id, fx string }{
		{"anthropic", "settings/models_anthropic"},
		{"google_ai", "settings/models_google_ai"},
		{"github_models", "settings/models_github_models"},
	} {
		fx := contract.Load(t, c.fx)
		got, found := m.List(context.Background(), c.id, false)
		if !found {
			t.Fatalf("%s not found", c.id)
		}
		raw, _ := json.Marshal(got)
		contract.AssertEqual(t, contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: raw}, fx.Response)
	}
}

func TestModelsOtherProvidersFallBackToDefaultModel(t *testing.T) {
	m := newModels(t, "")
	reg := mustLoad(t)
	for _, id := range reg.Providers.Keys() {
		if id == "openrouter" {
			continue
		}
		got, found := m.List(context.Background(), id, false)
		if !found {
			t.Fatalf("%s", id)
		}
		list := got.(map[string]any)["models"].([]ModelInfo)
		if _, static := reg.Models.Get(id); static {
			continue
		}
		p, _ := reg.Provider(id)
		want := []ModelInfo{{ID: p.DefaultModel, Name: p.DefaultModel, Context: 0, Free: true}}
		if !reflect.DeepEqual(list, want) {
			t.Errorf("%s fallback %v, want %v", id, list, want)
		}
	}
}

func TestModelsUnknown404(t *testing.T) {
	_, found := newModels(t, "").List(context.Background(), "nope", false)
	if found {
		t.Fatal("unknown provider must report not found")
	}
}

const orBody = `{"data":[
 {"id":"z/paid","name":"Zed","context_length":8000,"description":"d","pricing":{"prompt":"0.1","completion":"0.2"}},
 {"id":"b/free","name":"Bee","context_length":4000,"description":"` + "ééééé" + `","pricing":{"prompt":"0","completion":"0"}},
 {"id":"a/free","pricing":{"prompt":"0","completion":"0"}},
 {"id":"half","name":"Half","pricing":{"prompt":"0","completion":"1"}}
]}`

func TestModelsOpenRouterFreeOnlyFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(orBody))
	}))
	defer srv.Close()
	m := newModels(t, srv.URL+"/api/v1/models")

	all, _ := m.List(context.Background(), "openrouter", false)
	raw, _ := json.Marshal(all)
	var out struct {
		Models []struct {
			ID, Name, Description string
			Context               int
			Free                  bool
			Pricing               map[string]any
		}
		Total int
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, x := range out.Models {
		ids = append(ids, x.ID)
	}
	// free first, then by name: "a/free" (name defaults to id) < "Bee"? 'B' (0x42) < 'a' (0x61), so Bee first.
	if !reflect.DeepEqual(ids, []string{"b/free", "a/free", "half", "z/paid"}) || out.Total != 4 {
		t.Fatalf("order %v total %d", ids, out.Total)
	}
	if out.Models[1].Name != "a/free" || out.Models[1].Pricing["prompt"] != "0" {
		t.Errorf("defaults: %+v", out.Models[1])
	}
	if out.Models[0].Description != "ééééé" || out.Models[0].Context != 4000 {
		t.Errorf("entry %+v", out.Models[0])
	}

	free, _ := m.List(context.Background(), "openrouter", true)
	raw, _ = json.Marshal(free)
	var fo struct {
		Models []struct{ ID string }
		Total  int
	}
	_ = json.Unmarshal(raw, &fo)
	if fo.Total != 2 || len(fo.Models) != 2 {
		t.Fatalf("free_only: %s", raw)
	}
}

func TestModelsOpenRouterDescriptionTruncatedTo120Chars(t *testing.T) {
	long := make([]rune, 200)
	for i := range long {
		long[i] = 'é'
	}
	body, _ := json.Marshal(map[string]any{"data": []map[string]any{{"id": "x", "description": string(long)}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	got, _ := newModels(t, srv.URL).List(context.Background(), "openrouter", false)
	raw, _ := json.Marshal(got)
	var out struct {
		Models []struct{ Description string }
	}
	_ = json.Unmarshal(raw, &out)
	if n := len([]rune(out.Models[0].Description)); n != 120 {
		t.Fatalf("description %d chars", n)
	}
}

func TestModelsOpenRouterFailureReturnsEmptyWithError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer srv.Close()
	got, found := newModels(t, srv.URL).List(context.Background(), "openrouter", false)
	if !found {
		t.Fatal("openrouter exists")
	}
	raw, _ := json.Marshal(got)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if ms, ok := out["models"].([]any); !ok || len(ms) != 0 || out["error"] == nil || out["error"] == "" {
		t.Fatalf("got %s", raw)
	}
	if _, has := out["total"]; has {
		t.Fatal("error shape has no total")
	}
}
