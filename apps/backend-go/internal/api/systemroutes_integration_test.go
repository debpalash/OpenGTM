//go:build integration

package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func TestHardenedRequiresAuthAndAdmin(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	e := env{st: st}
	e.seed(t, "sysadmin", pythonHash, true, "admin", true)
	e.seed(t, "sysuser", pythonHash, true, "user", false)

	data := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := httpx.NewRouter()
	RegisterSystem(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Log: quietLog(),
		Flags: config.Flags{FilesAPIMode: config.FilesAPIModeHardened}, DataDir: data})
	h := r.Handler()

	call := func(method, path string, hdr map[string]string) int {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	routes := [][2]string{
		{"GET", "/api/system/stats"}, {"GET", "/api/files/list"},
		{"GET", "/api/files/download/f.txt"}, {"DELETE", "/api/files/delete/f.txt"},
	}
	for _, rt := range routes {
		if c := call(rt[0], rt[1], nil); c != http.StatusUnauthorized {
			t.Errorf("anonymous %v = %d, want 401", rt, c)
		}
	}
	user := bearer("sysuser")
	for _, rt := range routes { // every route (stats included, hardened) needs a global admin
		if c := call(rt[0], rt[1], user); c != http.StatusForbidden {
			t.Errorf("non-admin %v = %d, want 403", rt, c)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "f.txt")); err != nil {
		t.Fatalf("non-admin delete removed the file: %v", err)
	}
	admin := bearer("sysadmin")
	for _, rt := range routes {
		if c := call(rt[0], rt[1], admin); c != http.StatusOK {
			t.Errorf("admin %v = %d, want 200", rt, c)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "f.txt")); !os.IsNotExist(err) {
		t.Errorf("admin delete did not remove f.txt: %v", err)
	}
}
