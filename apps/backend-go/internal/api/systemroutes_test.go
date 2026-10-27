package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// sysHandler registers the system routes with their (always on) auth middleware.
func sysHandler(t *testing.T, mode, dataDir string) http.Handler {
	t.Helper()
	r := httpx.NewRouter()
	RegisterSystem(r, Deps{Log: quietLog(), Flags: config.Flags{FilesAPIMode: mode}, DataDir: dataDir, Config: config.APIConfig{DatabaseURL: "postgresql://x"}})
	return r.Handler()
}

func sysDo(h http.Handler, method, target string) (*httptest.ResponseRecorder, contract.Resp) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	hd := map[string]string{}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			hd[k] = v
		}
	}
	body := rec.Body.Bytes()
	if !strings.Contains(rec.Header().Get("Content-Type"), "json") && len(body) > 0 {
		body, _ = json.Marshal(string(body))
	}
	return rec, contract.Resp{Status: rec.Code, Headers: hd, Body: body}
}

// legacyTree builds the data tree with legacy/python/scripts/contract_system_tree.sh.
func legacyTree(t *testing.T) string {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	script, _ := filepath.Abs("../../../../legacy/python/scripts/contract_system_tree.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("legacy tree script not available: %v", err)
	}
	if out, err := exec.Command("sh", script, data).CombinedOutput(); err != nil {
		t.Fatalf("tree script: %v %s", err, out)
	}
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	return data
}

// S-M4: no mode value (including the removed "legacy") leaves a route without authentication.
func TestNoUnauthenticatedRouteInAnyMode(t *testing.T) {
	data := legacyTree(t)
	for _, mode := range []string{"", config.FilesAPIModeHardened, "legacy"} {
		h := sysHandler(t, mode, data)
		for _, c := range [][2]string{
			{"GET", "/api/system/stats"}, {"GET", "/api/files/list"},
			{"GET", "/api/files/download/alpha.txt"}, {"DELETE", "/api/files/delete/alpha.txt"}, {"DELETE", "/api/files/delete/sub"},
		} {
			if rec, _ := sysDo(h, c[0], c[1]); rec.Code != http.StatusUnauthorized {
				t.Errorf("mode %q %v = %d, want 401", mode, c, rec.Code)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(data, "sub")); err != nil {
		t.Errorf("anonymous delete removed sub: %v", err)
	}
}

func TestDownloadStreamsFileWithFilename(t *testing.T) {
	data := legacyTree(t)
	h := sysHandlerNoAuth(t, data)
	rec, _ := sysDo(h, "GET", "/api/files/download/sub/inner.md")
	if rec.Code != 200 || rec.Body.String() != "# inner\n" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename=inner.md` && cd != `attachment; filename="inner.md"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestNoInternalErrorTextLeaks(t *testing.T) {
	data := legacyTree(t)
	h := sysHandlerNoAuth(t, data)
	check := func(c [2]string) {
		t.Helper()
		rec, _ := sysDo(h, c[0], c[1])
		if rec.Code != 500 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Internal error"}` {
			t.Errorf("%v: %d %s", c, rec.Code, rec.Body.String())
		}
		lc := strings.ToLower(rec.Body.String())
		if strings.Contains(rec.Body.String(), data) || strings.Contains(lc, "permission") || strings.Contains(lc, "not a directory") || strings.Contains(lc, "errno") {
			t.Errorf("%v leaks error text: %s", c, rec.Body.String())
		}
	}
	// Root-safe failure injection: listing a regular file is ENOTDIR (500 in both modes).
	check([2]string{"GET", "/api/files/list?path=alpha.txt"})
	// Permission failures only exist for non-root users; run them whenever they can occur and say so otherwise.
	if os.Geteuid() == 0 {
		t.Log("running as root: permission-denied cases not applicable; ENOTDIR case above covers the leak assertions")
		return
	}
	locked := filepath.Join(data, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	for _, c := range [][2]string{{"GET", "/api/files/list?path=locked"}, {"GET", "/api/files/download/locked/f.txt"}, {"DELETE", "/api/files/delete/locked/f.txt"}} {
		check(c)
	}
}

type recHandler struct{ n *int }

func (h recHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h recHandler) Handle(context.Context, slog.Record) error { *h.n++; return nil }
func (h recHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h recHandler) WithGroup(string) slog.Handler             { return h }

func TestStatsCancelledRequestIsSilent(t *testing.T) {
	n := 0
	d := hardenedDeps(legacyTree(t))
	d.Log = slog.New(recHandler{&n})
	s := newSysRoutes(d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.stats(rec, httptest.NewRequest("GET", "/api/system/stats", nil).WithContext(ctx))
	if n != 0 || rec.Body.Len() != 0 {
		t.Errorf("cancelled stats logged %d records / wrote %q", n, rec.Body.String())
	}
}

func TestHardenedStatsBoundedAndCached(t *testing.T) {
	data := legacyTree(t)
	outside := filepath.Join(filepath.Dir(data), "big-sibling.bin")
	if err := os.WriteFile(outside, make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newSysRoutes(hardenedDeps(data))
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		s.stats(rec, httptest.NewRequest("GET", "/api/system/stats", nil))
		var m map[string]any
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
			t.Fatalf("stats: %d %s", rec.Code, rec.Body.String())
		}
		return m
	}
	first := get()
	if sz := first["project_size"].(float64); sz < 30 || sz > 200 {
		t.Errorf("project_size %v includes files outside DATA_DIR", sz)
	}
	if _, ok := first["project_size_partial"]; ok {
		t.Errorf("small tree flagged partial")
	}
	if err := os.WriteFile(filepath.Join(data, "grow.bin"), make([]byte, 7000), 0o644); err != nil {
		t.Fatal(err)
	}
	if again := get(); again["project_size"] != first["project_size"] {
		t.Errorf("second call within TTL was not served from cache")
	}
	s.cachedAt = time.Now().Add(-time.Hour)
	if fresh := get(); fresh["project_size"].(float64) < first["project_size"].(float64)+7000 {
		t.Errorf("expired cache not refreshed: %v", fresh["project_size"])
	}
}

func TestHardenedListTruncationHeader(t *testing.T) {
	data := legacyTree(t)
	r := httpx.NewRouter()
	s := newSysRoutes(hardenedDeps(data))
	s.fs = s.fs.WithListLimit(2)
	s.mount(r, nil, nil)
	rec, _ := sysDo(r.Handler(), "GET", "/api/files/list")
	var l []map[string]any
	if rec.Header().Get("X-Files-Truncated") != "true" || json.Unmarshal(rec.Body.Bytes(), &l) != nil || len(l) != 2 {
		t.Errorf("truncation: header=%q body=%s", rec.Header().Get("X-Files-Truncated"), rec.Body.String())
	}
	rec, _ = sysDo(r.Handler(), "GET", "/api/files/list?path=sub")
	if rec.Header().Get("X-Files-Truncated") != "" {
		t.Errorf("small listing flagged truncated")
	}
}

func TestHardenedHTTPTraversalNeverTouchesOutside(t *testing.T) {
	data := legacyTree(t)
	tmp := filepath.Dir(data)
	evil := filepath.Join(tmp, "data-evil")
	_ = os.MkdirAll(evil, 0o755)
	_ = os.WriteFile(filepath.Join(evil, "secret.txt"), []byte("evil-secret"), 0o644)
	_ = os.Symlink("/etc", filepath.Join(data, "etclink"))
	h := sysHandlerNoAuth(t, data)
	targets := []string{
		"../data-evil/secret.txt", "%2e%2e/data-evil/secret.txt", "%2e%2e%2fdata-evil%2fsecret.txt",
		"/etc/passwd", "etclink/passwd", "sub/../../data-evil/secret.txt",
	}
	for _, p := range targets {
		for _, c := range [][2]string{{"GET", "/api/files/download/" + p}, {"DELETE", "/api/files/delete/" + p}, {"GET", "/api/files/list?path=" + p}} {
			rec, _ := sysDo(h, c[0], c[1])
			if rec.Code == 200 || strings.Contains(rec.Body.String(), "evil-secret") || strings.Contains(rec.Body.String(), "root:") {
				t.Errorf("%v: %d %.80s", c, rec.Code, rec.Body.String())
			}
		}
	}
	if _, err := os.Stat(filepath.Join(evil, "secret.txt")); err != nil {
		t.Errorf("outside secret removed: %v", err)
	}
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Errorf("/etc/passwd gone?: %v", err)
	}
	// Direct handler route (bypasses mux dot-segment cleaning) answers the documented 403s.
	for _, c := range []struct{ method, path, want string }{
		{"GET", "download", "Access denied"}, {"GET", "list", "Invalid path: Access denied"}, {"DELETE", "delete", "Invalid path: Access denied"},
	} {
		rec := directFiles(t, data, c.method, c.path, "../data-evil/secret.txt")
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), `"`+c.want+`"`) {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestHardenedDeleteBehaviours(t *testing.T) {
	data := legacyTree(t)
	h := sysHandlerNoAuth(t, data)
	rec, _ := sysDo(h, "DELETE", "/api/files/delete/")
	if rec.Code == 200 {
		t.Errorf("deleting the data dir root answered 200")
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("data dir removed: %v", err)
	}
	rec, _ = sysDo(h, "DELETE", "/api/files/delete/sub")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"Directory is not empty"`) {
		t.Errorf("non-empty dir: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = sysDo(h, "DELETE", "/api/files/delete/emptydir")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"message":"File deleted","status":"success"}` {
		t.Errorf("empty dir: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = sysDo(h, "DELETE", "/api/files/delete/nope.txt")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"File not found or could not be deleted"`) {
		t.Errorf("missing: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = sysDo(h, "GET", "/api/files/download/nope.txt")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"File not found"`) {
		t.Errorf("download missing: %d %s", rec.Code, rec.Body.String())
	}
}

func hardenedDeps(data string) Deps {
	return Deps{Log: quietLog(), Flags: config.Flags{FilesAPIMode: config.FilesAPIModeHardened}, DataDir: data}
}

// sysHandlerNoAuth mounts the hardened handlers without the auth middleware (covered by the
// integration test TestHardenedRequiresAuthAndAdmin).
func sysHandlerNoAuth(t *testing.T, data string) http.Handler {
	t.Helper()
	r := httpx.NewRouter()
	newSysRoutes(hardenedDeps(data)).mount(r, nil, nil)
	return r.Handler()
}

// directFiles calls a handler with a raw path value, bypassing the mux's dot-segment cleaning.
func directFiles(t *testing.T, data, method, route, p string) *httptest.ResponseRecorder {
	t.Helper()
	s := newSysRoutes(hardenedDeps(data))
	target := "/x"
	if route == "list" {
		target = "/x?path=" + p
	}
	req := httptest.NewRequest(method, target, nil)
	req.SetPathValue("file_path", p)
	rec := httptest.NewRecorder()
	switch route {
	case "download":
		s.download(rec, req)
	case "list":
		s.list(rec, req)
	default:
		s.remove(rec, req)
	}
	return rec
}

func TestRegisterSystemRoutes(t *testing.T) {
	r := httpx.NewRouter()
	RegisterSystem(r, Deps{Log: quietLog(), Flags: config.Flags{FilesAPIMode: config.FilesAPIModeHardened}, DataDir: t.TempDir()})
	got := strings.Join(r.Patterns(), "|")
	want := "DELETE /api/files/delete/{file_path...}|GET /api/files/download/{file_path...}|GET /api/files/list|GET /api/system/stats"
	if got != want {
		t.Errorf("patterns = %s", got)
	}
}
