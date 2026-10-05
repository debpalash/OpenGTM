package pluginrun

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/kernels"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/fixture"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// fakeLegacy answers /api/auth/workspace-context for three callers.
func fakeLegacy(t *testing.T) *httptest.Server {
	t.Helper()
	identities := map[string]authz.Workspace{
		"editor-a": {UserID: "1", Username: "ed", WorkspaceID: "ws-a", Slug: "a", Role: "editor"},
		"viewer-a": {UserID: "2", Username: "vi", WorkspaceID: "ws-a", Slug: "a", Role: "viewer"},
		"editor-b": {UserID: "3", Username: "eb", WorkspaceID: "ws-b", Slug: "b", Role: "editor"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != authz.ContextPath {
			http.NotFound(w, r)
			return
		}
		id, ok := identities[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"Could not validate credentials"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(id)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type harness struct {
	t     *testing.T
	mux   *http.ServeMux
	queue *queue.Queue
}

// gatedExtractor blocks every extraction until gate is closed, so a test can
// act while a run is executing.
type gatedExtractor struct {
	inner   declarative.Extractor
	entered chan struct{}
	gate    chan struct{}
}

func (g *gatedExtractor) Extract(ctx context.Context, req kernels.ExtractRequest) (kernels.ExtractResult, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.gate:
	case <-ctx.Done():
		return kernels.ExtractResult{}, ctx.Err()
	}
	return g.inner.Extract(ctx, req)
}

func newHarness(t *testing.T) *harness { return newGatedHarness(t, nil) }

func newGatedHarness(t *testing.T, gate *gatedExtractor) *harness {
	t.Helper()
	owner := dbtest.OwnerURL(t)
	dbtest.Migrate(t, owner)
	pool := dbtest.Pool(t, dbtest.AppURL(t, owner), 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs WHERE type = $1`, JobType); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dbtest.RepoRoot(t), "plugins", "examples")
	catalog := LoadCatalog(config.Plugins{Dirs: []string{root}, SignaturePolicy: "optional"})
	if _, ok := catalog.Get("acme_team_page"); !ok {
		t.Fatalf("example scraper not loaded; errors: %+v", catalog.Errors)
	}

	// Replay the example's recorded pages instead of touching the network.
	c, err := fixture.LoadCase(filepath.Join(root, "declarative-scraper", "fixtures", "basic", "case.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := egress.New(egress.Options{Transport: fixture.NewTransport(c), ReplayWithoutRateLimit: true})
	if err != nil {
		t.Fatal(err)
	}
	k, err := kernels.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })

	var extractor declarative.Extractor = k
	if gate != nil {
		gate.inner = k
		extractor = gate
	}
	reg := queue.NewRegistry()
	NewWorker(pool, catalog, NewRunner(client, extractor, nil), nil).Register(reg)
	q := queue.New(pool, reg, queue.Options{
		Concurrency: 2, IdlePoll: 20 * time.Millisecond, ClaimCheckInterval: 50 * time.Millisecond,
	})

	az, err := authz.New(fakeLegacy(t).URL, authz.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewAPI(pool, catalog, az).Mount(mux)
	return &harness{t: t, mux: mux, queue: q}
}

func (h *harness) startWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.queue.Run(ctx) }()
	h.t.Cleanup(func() { cancel(); <-done })
}

func (h *harness) do(token, method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	validateAgainstSpec(h.t, h.mux, req, rec) // every response must match openapi.v2.yaml
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (h *harness) waitStatus(token, id string, want ...string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, run := h.do(token, "GET", "/api/v2/plugin-runs/"+id, nil)
		for _, w := range want {
			if run["status"] == w {
				return run
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("run %s stuck in %v (want %v): %+v", id, run["status"], want, run)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPluginRunEndToEnd(t *testing.T) {
	h := newHarness(t)

	code, body := h.do("editor-a", "GET", "/api/v2/plugins", nil)
	if code != http.StatusOK || !strings.Contains(mustJSON(body), `"acme_team_page"`) {
		t.Fatalf("catalog: %d %v", code, body)
	}

	code, run := h.do("editor-a", "POST", "/api/v2/plugin-runs",
		map[string]any{"plugin": "acme_team_page", "inputs": map[string]any{"domain": "acme.example"}})
	if code != http.StatusAccepted || run["status"] != "pending" || run["job_id"] == nil {
		t.Fatalf("create: %d %+v", code, run)
	}
	id := run["id"].(string)

	h.startWorker()
	done := h.waitStatus("editor-a", id, "completed", "failed")
	if done["status"] != "completed" {
		t.Fatalf("run failed: %+v", done)
	}
	stats := done["stats"].(map[string]any)
	if stats["records"] != float64(3) || stats["pages"] != float64(2) {
		t.Fatalf("stats: %+v", stats)
	}

	code, res := h.do("editor-a", "GET", "/api/v2/plugin-runs/"+id+"/results", nil)
	if code != http.StatusOK || res["total"] != float64(3) {
		t.Fatalf("results: %d %+v", code, res)
	}
	first := res["results"].([]any)[0].(map[string]any)
	data := first["data"].(map[string]any)
	if data["full_name"] != "Ada Lovelace" || data["linkedin_url"] != "https://www.linkedin.com/in/ada-example" {
		t.Fatalf("first record: %+v", first)
	}
	ev := first["evidence"].(map[string]any)
	if ev["url"] != "https://acme.example/team" || ev["body_sha256"] == "" {
		t.Fatalf("evidence: %+v", ev)
	}

	for _, bad := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if code, _ := h.do("editor-a", "GET", "/api/v2/plugin-runs/"+bad+"/results", nil); code != http.StatusNotFound {
			t.Fatalf("results for %q: %d", bad, code)
		}
	}

	// Another tenant sees nothing, through both the API and RLS.
	if code, _ := h.do("editor-b", "GET", "/api/v2/plugin-runs/"+id, nil); code != http.StatusNotFound {
		t.Fatalf("cross-tenant get: %d", code)
	}
	if code, _ := h.do("editor-b", "GET", "/api/v2/plugin-runs/"+id+"/results", nil); code != http.StatusNotFound {
		t.Fatalf("cross-tenant results: %d", code)
	}
	_, list := h.do("editor-b", "GET", "/api/v2/plugin-runs", nil)
	if n := len(list["runs"].([]any)); n != 0 {
		t.Fatalf("cross-tenant list returned %d runs", n)
	}
}

func TestPluginRunAuthorizationAndValidation(t *testing.T) {
	h := newHarness(t)
	create := func(token string, body any) int {
		code, _ := h.do(token, "POST", "/api/v2/plugin-runs", body)
		return code
	}
	if c := create("viewer-a", map[string]any{"plugin": "acme_team_page", "inputs": map[string]any{"domain": "x.example"}}); c != http.StatusForbidden {
		t.Fatalf("viewer create: %d", c)
	}
	if c := create("nobody", map[string]any{"plugin": "acme_team_page"}); c != http.StatusUnauthorized {
		t.Fatalf("anonymous create: %d", c)
	}
	if c := create("editor-a", map[string]any{"plugin": "not_installed"}); c != http.StatusNotFound {
		t.Fatalf("unknown plugin: %d", c)
	}
	if c := create("editor-a", map[string]any{"plugin": "acme_team_page", "inputs": map[string]any{"domain": 7}}); c != http.StatusUnprocessableEntity {
		t.Fatalf("invalid inputs: %d", c)
	}
	if c := create("editor-a", map[string]any{"plugin": "acme_team_page", "extra": true}); c != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", c)
	}
}

func TestCancelledRunIsNeverExecuted(t *testing.T) {
	h := newHarness(t)
	_, run := h.do("editor-a", "POST", "/api/v2/plugin-runs",
		map[string]any{"plugin": "acme_team_page", "inputs": map[string]any{"domain": "acme.example"}})
	id := run["id"].(string)

	if code, _ := h.do("viewer-a", "POST", "/api/v2/plugin-runs/"+id+"/cancel", nil); code != http.StatusForbidden {
		t.Fatalf("viewer cancel: %d", code)
	}
	code, cancelled := h.do("editor-a", "POST", "/api/v2/plugin-runs/"+id+"/cancel", nil)
	if code != http.StatusOK || cancelled["status"] != "cancelled" {
		t.Fatalf("cancel: %d %+v", code, cancelled)
	}
	if code, _ := h.do("editor-a", "POST", "/api/v2/plugin-runs/"+id+"/cancel", nil); code != http.StatusConflict {
		t.Fatalf("second cancel: %d", code)
	}

	h.startWorker()
	time.Sleep(500 * time.Millisecond)
	final := h.waitStatus("editor-a", id, "cancelled", "completed", "failed")
	if final["status"] != "cancelled" || final["started_at"] != nil {
		t.Fatalf("cancelled run was executed: %+v", final)
	}
}

func TestCancelWhileRunningKeepsCancellationAndDropsResults(t *testing.T) {
	gate := &gatedExtractor{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	h := newGatedHarness(t, gate)
	_, run := h.do("editor-a", "POST", "/api/v2/plugin-runs",
		map[string]any{"plugin": "acme_team_page", "inputs": map[string]any{"domain": "acme.example"}})
	id := run["id"].(string)

	h.startWorker()
	select {
	case <-gate.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("run never reached extraction")
	}
	if got := h.waitStatus("editor-a", id, "running"); got["started_at"] == nil {
		t.Fatalf("running run has no started_at: %+v", got)
	}
	if code, _ := h.do("editor-a", "POST", "/api/v2/plugin-runs/"+id+"/cancel", nil); code != http.StatusOK {
		t.Fatalf("cancel running: %d", code)
	}
	close(gate.gate)

	// Give the worker time to notice the cancelled lease and finish.
	time.Sleep(time.Second)
	final := h.waitStatus("editor-a", id, "cancelled", "completed", "failed")
	if final["status"] != "cancelled" {
		t.Fatalf("cancellation overwritten: %+v", final)
	}
	_, res := h.do("editor-a", "GET", "/api/v2/plugin-runs/"+id+"/results", nil)
	if res["total"] != float64(0) {
		t.Fatalf("results committed after cancellation: %+v", res)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
