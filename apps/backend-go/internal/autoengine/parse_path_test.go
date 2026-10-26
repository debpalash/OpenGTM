package autoengine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// These tests drive autoactions.Execute with an action parsed by the engine's
// own parseActions (ordered *wbcrud.Obj values), not a hand-built map.

type sink struct {
	mu   sync.Mutex
	body string
	hdr  http.Header
	hits int
}

func webhookEnv(t *testing.T) (autoactions.Deps, *sink, string) {
	t.Helper()
	s := &sink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.body, s.hdr, s.hits = string(b), r.Header.Clone(), s.hits+1
		s.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]
	d := autoactions.Deps{Webhook: autoactions.NewWebhookDeps("test", []string{host}, autoactionsSettings(), nil, nil)}
	d.Webhook.Timeout = 5 * time.Second
	return d, s, srv.URL
}

func execParsed(t *testing.T, d autoactions.Deps, actionJSON string) autoactions.Result {
	t.Helper()
	acts := parseActions([]byte("[" + actionJSON + "]"))
	lead := map[string]any{"name": "Ann", "email": "a@x.io"}
	return autoactions.Execute(context.Background(), d, "ws", acts[0], "wb", "1", nil, lead, nil, "k")
}

func TestParsedWebhookBodies(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"object", `{"z":"{name}","a":"hi"}`, `{"z":"Ann","a":"hi"}`},
		{"list of objects", `[{"x":"{name}"},{"b":1,"a":"{email}"}]`, `[{"x":"Ann"},{"b":1,"a":"a@x.io"}]`},
		{"nested", `{"o":{"n":"{name}","l":["{email}",{"k":"{name}"}]},"first":2}`, `{"o":{"n":"Ann","l":["a@x.io",{"k":"Ann"}]},"first":2}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, s, u := webhookEnv(t)
			res := execParsed(t, d, `{"type":"webhook","config":{"url":"`+u+`","body":`+c.body+`}}`)
			if res.Status != "success" {
				t.Fatalf("%+v", res)
			}
			if s.body != c.want {
				t.Fatalf("sent %s, want %s", s.body, c.want)
			}
		})
	}
}

func TestParsedWebhookHeadersObject(t *testing.T) {
	d, s, u := webhookEnv(t)
	res := execParsed(t, d, `{"type":"webhook","config":{"url":"`+u+`","headers":{"X-Who":"{name}"},"body":{"a":1}}}`)
	if res.Status != "success" || s.hdr.Get("X-Who") != "Ann" {
		t.Fatalf("%+v %v", res, s.hdr)
	}
}

func TestParsedPushCRMFieldMapOrder(t *testing.T) {
	var got crmpush.Config
	d := autoactions.Deps{Push: func(_ context.Context, _ string, cfg, _ map[string]any, _ string, _ int64) crmpush.Result {
		got = crmpush.ParseConfig(cfg)
		return crmpush.Result{Success: true, Value: "ok"}
	}}
	res := execParsed(t, d, `{"type":"push_crm","config":{"type":"hubspot","field_map":{"z_last":"Z","a_first":"A","m":"M"}}}`)
	if res.Status != "success" {
		t.Fatalf("%+v", res)
	}
	var keys []string
	for _, p := range got.FieldMap {
		keys = append(keys, p.Key)
	}
	if !reflect.DeepEqual(keys, []string{"z_last", "a_first", "m"}) {
		t.Fatalf("field_map order: %v", keys)
	}
}

func TestParsedReEnrichColumnIDs(t *testing.T) {
	var gotCols []string
	d := autoactions.Deps{RunRows: func(_ context.Context, _, _ string, cols []string, _ []any, _ time.Duration) (workbook.RunSummary, error) {
		gotCols = cols
		return workbook.RunSummary{Completed: 2}, nil
	}}
	acts := parseActions([]byte(`[{"type":"re_enrich","config":{"column_ids":["c2","c1"]}}]`))
	cols := []autoactions.Column{colOf("c1"), colOf("c2")}
	res := autoactions.Execute(context.Background(), d, "ws", acts[0], "wb", "1", nil, map[string]any{}, cols, "k")
	if res.Status != "success" || !reflect.DeepEqual(gotCols, []string{"c2", "c1"}) {
		t.Fatalf("%+v %v", res, gotCols)
	}
	if c := autoactions.ProjectCost(acts[0], cols, nil); c < 0 {
		t.Fatal(c)
	}
}

func autoactionsSettings() config.AutomationsSettings { return config.AutomationsSettings{} }

func colOf(id string) autoactions.Column { return colgraph.MakeCol("id", id, "type", "text") }
