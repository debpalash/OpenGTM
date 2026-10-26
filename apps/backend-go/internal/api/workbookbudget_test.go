//go:build integration

package api

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Add rows and workbook create/update share the import byte budget (final-review I1): a held
// 64 MiB upload leaves no budget, so another buffered body gets 503 instead of more memory.
func TestBufferedBodiesShareImportBudget(t *testing.T) {
	old := importBudgetWait
	importBudgetWait = 150 * time.Millisecond
	t.Cleanup(func() { importBudgetWait = old })
	e := newRowsEnv(t)
	e.seed(t)
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true, ImportMemoryBudgetMB: 64}}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	RegisterWorkbookImport(r, d)
	RegisterWorkbookRows(r, d)
	RegisterWorkbooks(r, d)
	e.h = r.Handler()
	alice := e.users(t)["alice"]

	pr, pw := io.Pipe()
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() { held <- e.post("/api/workbooks/wb-alpha-1/import", pr, -1, alice) }()
	time.Sleep(100 * time.Millisecond)

	add := `{"rows":[{"a":1}]}`
	if rec := e.post("/api/workbooks/wb-alpha-1/rows", strings.NewReader(add), int64(len(add)), alice); rec.Code != 503 {
		t.Errorf("add rows under a full budget: %d %s", rec.Code, rec.Body.String())
	}
	create := `{"name":"x"}`
	if rec := e.post("/api/workbooks/", strings.NewReader(create), int64(len(create)), alice); rec.Code != 503 {
		t.Errorf("create under a full budget: %d %s", rec.Code, rec.Body.String())
	}
	_, _ = pw.Write([]byte(`{"rows":[{"Company":"Held Co"}]}`))
	_ = pw.Close()
	<-held
	if rec := e.post("/api/workbooks/wb-alpha-1/rows", strings.NewReader(add), int64(len(add)), alice); rec.Code != 200 {
		t.Errorf("add rows after release: %d %s", rec.Code, rec.Body.String())
	}
}
