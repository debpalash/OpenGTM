package api

import (
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

// sysHandler registers the system routes without auth middleware (legacy mode) or with it
// (hardened); unit tests exercise legacy and call handlers directly for hardened behaviour.
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

func TestLegacyModeMatchesPythonFixtures(t *testing.T) {
	data := legacyTree(t)
	h := sysHandler(t, config.FilesAPIModeLegacy, data)
	// Order matters: later cases see the deletions recorded by FastAPI.
	for _, name := range []string{
		"list_root", "list_sub", "list_nested", "list_empty_dir", "list_missing", "list_traversal",
		"download_text", "download_nested", "download_missing", "download_dir",
		"delete_missing", "delete_file", "delete_file_again", "delete_dir_recursive", "list_root_after",
	} {
		fx := contract.Load(t, "system/"+name)
		_, got := sysDo(h, fx.Request.Method, fx.Request.Path)
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	fx := contract.Load(t, "system/stats")
	_, got := sysDo(h, "GET", fx.Request.Path)
	contract.AssertEqual(t, got, fx.Response, contract.IgnoreShapeOf("$.disk_total", "$.disk_used", "$.disk_free", "$.project_size", "$.db_size"))
	var keys []string
	dec := json.NewDecoder(strings.NewReader(string(got.Body)))
	dec.Token()
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var v json.RawMessage
		dec.Decode(&v)
	}
	if strings.Join(keys, ",") != "disk_total,disk_used,disk_free,project_size,db_size" {
		t.Errorf("stats key order = %v", keys)
	}
}

func TestDownloadStreamsFileWithFilename(t *testing.T) {
	data := legacyTree(t)
	for _, mode := range []string{config.FilesAPIModeLegacy, config.FilesAPIModeHardened} {
		var h http.Handler = sysHandler(t, mode, data)
		if mode == config.FilesAPIModeHardened {
			h = sysHandlerNoAuth(t, data)
		}
		rec, _ := sysDo(h, "GET", "/api/files/download/sub/inner.md")
		if rec.Code != 200 || rec.Body.String() != "# inner\n" {
			t.Fatalf("%s: %d %q", mode, rec.Code, rec.Body.String())
		}
		if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename=inner.md` && cd != `attachment; filename="inner.md"` {
			t.Errorf("%s: Content-Disposition = %q", mode, cd)
		}
	}
}

func TestNoInternalErrorTextLeaks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission errors need a non-root user")
	}
	data := legacyTree(t)
	secretDir := filepath.Join(data, "locked")
	if err := os.Mkdir(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(secretDir, "f.txt"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secretDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(secretDir, 0o755) })
	h := sysHandlerNoAuth(t, data)
	for _, c := range [][2]string{{"GET", "/api/files/list?path=locked"}, {"GET", "/api/files/download/locked/f.txt"}, {"DELETE", "/api/files/delete/locked/f.txt"}} {
		rec, _ := sysDo(h, c[0], c[1])
		if rec.Code != 500 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Internal error"}` {
			t.Errorf("%v: %d %s", c, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), data) || strings.Contains(strings.ToLower(rec.Body.String()), "permission") {
			t.Errorf("%v leaks error text: %s", c, rec.Body.String())
		}
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
		"/etc/passwd", "etclink/passwd", "sub/../../data-evil/secret.txt", "..%5cdata-evil",
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
	for _, mode := range []string{config.FilesAPIModeHardened, config.FilesAPIModeLegacy} {
		r := httpx.NewRouter()
		RegisterSystem(r, Deps{Log: quietLog(), Flags: config.Flags{FilesAPIMode: mode}, DataDir: t.TempDir()})
		got := strings.Join(r.Patterns(), "|")
		want := "DELETE /api/files/delete/{file_path...}|GET /api/files/download/{file_path...}|GET /api/files/list|GET /api/system/stats"
		if got != want {
			t.Errorf("%s patterns = %s", mode, got)
		}
	}
}
