//go:build integration

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/demoseed"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcecatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type obEnv struct {
	mcpEnv
	app *settings.App
	res *secrets.Resolver
}

type obState struct {
	Steps []struct {
		ID   string `json:"id"`
		Done bool   `json:"done"`
	} `json:"steps"`
	DoneCount        int     `json:"done_count"`
	Total            int     `json:"total"`
	Complete         bool    `json:"complete"`
	SampleWorkbookID *string `json:"sample_workbook_id"`
}

func (s obState) done(id string) bool {
	for _, st := range s.Steps {
		if st.ID == id {
			return st.Done
		}
	}
	return false
}

func obCleanup(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM workbook_enrichments WHERE workspace_id IN ('ob-ws-a','ob-ws-b')`,
		`DELETE FROM workbook_rows WHERE workspace_id IN ('ob-ws-a','ob-ws-b')`,
		`DELETE FROM workbooks WHERE workspace_id IN ('ob-ws-a','ob-ws-b')`,
		`DELETE FROM workspace_settings WHERE workspace_id IN ('ob-ws-a','ob-ws-b')`,
		`DELETE FROM app_settings WHERE key LIKE 'SOURCE\_%\_ENABLED' OR key LIKE '%\_API\_KEY' OR key LIKE 'CUSTOM\_OPENAI\_%'`,
	} {
		ownerExec(t, q)
	}
}

func newOBEnv(t *testing.T) obEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := settings.Load()
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Envelope: envl, Registry: reg, Getenv: getenv, Config: config.APIConfig{}}
	RegisterOnboarding(r, d)
	e := obEnv{
		mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool},
		app:    &settings.App{Store: secrets.NewPgStore(pool), Env: envl, Secret: reg.SecretSet(), Getenv: getenv},
		res:    secrets.NewResolver(envl, secrets.NewPgStore(pool), getenv),
	}
	obCleanup(t)
	e.seed(t, "ob-alice", pythonHash, true, "user", false)
	e.seed(t, "ob-edie", pythonHash, true, "user", false)
	e.seed(t, "ob-vic", pythonHash, true, "user", false)
	e.seed(t, "ob-bob", pythonHash, true, "user", false)
	e.workspace(t, "ob-ws-a", e.mustUserByName(t, "ob-alice").ID)
	e.workspace(t, "ob-ws-b", e.mustUserByName(t, "ob-bob").ID)
	e.member(t, "ob-ws-a", e.mustUserByName(t, "ob-alice").ID, "admin")
	e.member(t, "ob-ws-a", e.mustUserByName(t, "ob-edie").ID, "editor")
	e.member(t, "ob-ws-a", e.mustUserByName(t, "ob-vic").ID, "viewer")
	e.member(t, "ob-ws-b", e.mustUserByName(t, "ob-bob").ID, "admin")
	t.Cleanup(func() { obCleanup(t) }) // registered after the workspaces: cleanups run last-in first, workbooks must go before them
	return e
}

func (e obEnv) call(method, path, who, ws string) (int, []byte) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do(method, path, "", "", hdr)
	return rec.Code, rec.Body.Bytes()
}

func (e obEnv) state(t *testing.T, who, ws string) obState {
	t.Helper()
	code, body := e.call("GET", "/api/onboarding", who, ws)
	if code != 200 {
		t.Fatalf("GET /api/onboarding as %s in %s = %d %s", who, ws, code, body)
	}
	var s obState
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e obEnv) disableAllSources(t *testing.T) {
	t.Helper()
	for _, src := range sourcecatalog.Sources() {
		if err := sourcecatalog.SetEnabled(context.Background(), e.app, src.ID, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOnboardingStepsFlipAsDataIsAdded(t *testing.T) {
	e := newOBEnv(t)
	ctx := context.Background()
	e.disableAllSources(t)

	s := e.state(t, "ob-alice", "ob-ws-a")
	if s.DoneCount != 0 || s.Total != 4 || s.Complete || len(s.Steps) != 4 || s.SampleWorkbookID != nil {
		t.Fatalf("fresh workspace: %+v", s)
	}
	for i, id := range []string{"llm_provider", "data_source", "data", "enrichment"} {
		if s.Steps[i].ID != id {
			t.Fatalf("step %d = %s, want %s", i, s.Steps[i].ID, id)
		}
	}

	// 1. an LLM provider: a global key
	if err := e.app.Set(ctx, "OPENROUTER_API_KEY", "sk-or-test-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if s = e.state(t, "ob-alice", "ob-ws-a"); !s.done("llm_provider") || s.DoneCount != 1 {
		t.Fatalf("after global key: %+v", s)
	}
	// 2. a data source
	if err := sourcecatalog.SetEnabled(ctx, e.app, "duckduckgo", true); err != nil {
		t.Fatal(err)
	}
	if s = e.state(t, "ob-alice", "ob-ws-a"); !s.done("data_source") || s.DoneCount != 2 {
		t.Fatalf("after source: %+v", s)
	}
	// 3. a workbook: the sample
	code, body := e.call("POST", "/api/onboarding/sample", "ob-edie", "ob-ws-a")
	if code != 201 {
		t.Fatalf("sample load = %d %s", code, body)
	}
	var made struct {
		WorkbookID string `json:"workbook_id"`
		Name       string `json:"name"`
		Created    bool   `json:"created"`
	}
	if err := json.Unmarshal(body, &made); err != nil {
		t.Fatal(err)
	}
	if !made.Created || made.WorkbookID != demoseed.SampleWorkbookID("ob-ws-a") || made.Name != demoseed.SampleName {
		t.Fatalf("sample body %+v", made)
	}
	s = e.state(t, "ob-alice", "ob-ws-a")
	if !s.done("data") || s.done("enrichment") || s.DoneCount != 3 || s.SampleWorkbookID == nil || *s.SampleWorkbookID != made.WorkbookID {
		t.Fatalf("after sample: %+v", s)
	}
	// 4. an enrichment that completed
	ownerExec(t, `INSERT INTO workbook_enrichments (workbook_id, lead_id, column_id, value, status, provider, workspace_id)
	  VALUES ($1, 1, 'find_email', 'a@b.co', 'complete', 'p', 'ob-ws-a')`, made.WorkbookID)
	if s = e.state(t, "ob-alice", "ob-ws-a"); !s.done("enrichment") || !s.Complete || s.DoneCount != 4 {
		t.Fatalf("after enrichment: %+v", s)
	}
	// undoing a step flips it back: nothing is stored
	ownerExec(t, `DELETE FROM workbook_enrichments WHERE workspace_id = 'ob-ws-a'`)
	if s = e.state(t, "ob-alice", "ob-ws-a"); s.done("enrichment") || s.Complete {
		t.Fatalf("after undo: %+v", s)
	}
}

func TestOnboardingKeylessCustomEndpointCompletesLLMStep(t *testing.T) {
	e := newOBEnv(t)
	ctx := context.Background()
	if e.state(t, "ob-alice", "ob-ws-a").done("llm_provider") {
		t.Fatal("llm step done with nothing configured")
	}
	// a base URL alone is not enough: the model is needed too
	if err := e.app.Set(ctx, "CUSTOM_OPENAI_BASE_URL", "http://localhost:11434/v1"); err != nil {
		t.Fatal(err)
	}
	if e.state(t, "ob-alice", "ob-ws-a").done("llm_provider") {
		t.Fatal("llm step done with a base URL but no model")
	}
	if err := e.app.Set(ctx, "CUSTOM_OPENAI_MODEL", "llama3"); err != nil {
		t.Fatal(err)
	}
	if !e.state(t, "ob-alice", "ob-ws-a").done("llm_provider") {
		t.Fatal("keyless custom endpoint did not complete the llm step")
	}
}

func TestOnboardingTenantIsolation(t *testing.T) {
	e := newOBEnv(t)
	ctx := context.Background()
	e.disableAllSources(t)
	// a key saved for workspace A only
	if err := e.res.Set(ctx, "ob-ws-a", "OPENROUTER_API_KEY", "sk-or-ws-a-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if code, body := e.call("POST", "/api/onboarding/sample", "ob-alice", "ob-ws-a"); code != 201 {
		t.Fatalf("sample A = %d %s", code, body)
	}
	ownerExec(t, `INSERT INTO workbook_enrichments (workbook_id, lead_id, column_id, value, status, provider, workspace_id)
	  VALUES ($1, 1, 'find_email', 'a@b.co', 'complete', 'p', 'ob-ws-a')`, demoseed.SampleWorkbookID("ob-ws-a"))

	a := e.state(t, "ob-alice", "ob-ws-a")
	if !a.done("llm_provider") || !a.done("data") || !a.done("enrichment") {
		t.Fatalf("workspace A: %+v", a)
	}
	b := e.state(t, "ob-bob", "ob-ws-b")
	if b.DoneCount != 0 || b.SampleWorkbookID != nil {
		t.Fatalf("workspace A's data completed workspace B's steps: %+v", b)
	}
	// B loads its own sample under its own id; A's is untouched
	code, body := e.call("POST", "/api/onboarding/sample", "ob-bob", "ob-ws-b")
	if code != 201 {
		t.Fatalf("sample B = %d %s", code, body)
	}
	b = e.state(t, "ob-bob", "ob-ws-b")
	if !b.done("data") || b.done("enrichment") || b.SampleWorkbookID == nil || *b.SampleWorkbookID == *a.SampleWorkbookID {
		t.Fatalf("workspace B after its sample: %+v", b)
	}
	// a caller cannot read another workspace's state by naming it
	if code, _ := e.call("GET", "/api/onboarding", "ob-bob", "ob-ws-a"); code != 403 {
		t.Fatalf("non-member reading workspace A = %d, want 403", code)
	}
}

func TestOnboardingSampleIsIdempotent(t *testing.T) {
	e := newOBEnv(t)
	code1, body1 := e.call("POST", "/api/onboarding/sample", "ob-edie", "ob-ws-a")
	code2, body2 := e.call("POST", "/api/onboarding/sample", "ob-alice", "ob-ws-a")
	if code1 != 201 || code2 != 200 {
		t.Fatalf("first %d %s, second %d %s", code1, body1, code2, body2)
	}
	var m1, m2 map[string]any
	_ = json.Unmarshal(body1, &m1)
	_ = json.Unmarshal(body2, &m2)
	if m1["workbook_id"] != m2["workbook_id"] || m2["created"] != false {
		t.Fatalf("second call did not return the existing sample: %v vs %v", m1, m2)
	}
	wbs := ownerCount(t, `SELECT count(*) FROM workbooks WHERE workspace_id = 'ob-ws-a'`)
	rows := ownerCount(t, `SELECT count(*) FROM workbook_rows WHERE workspace_id = 'ob-ws-a'`)
	if wbs != 1 || rows != 25 {
		t.Fatalf("workbooks %d rows %d, want 1 and 25", wbs, rows)
	}
}

func TestOnboardingRoleAndAuthGates(t *testing.T) {
	e := newOBEnv(t)
	if code, body := e.call("POST", "/api/onboarding/sample", "ob-vic", "ob-ws-a"); code != 403 {
		t.Fatalf("viewer POST sample = %d %s, want 403", code, body)
	}
	if n := ownerCount(t, `SELECT count(*) FROM workbooks WHERE workspace_id = 'ob-ws-a'`); n != 0 {
		t.Fatalf("viewer's refused request still created %d workbooks", n)
	}
	// a viewer may read the checklist
	if code, _ := e.call("GET", "/api/onboarding", "ob-vic", "ob-ws-a"); code != 200 {
		t.Fatalf("viewer GET = %d, want 200", code)
	}
	for _, c := range [][2]string{{"GET", "/api/onboarding"}, {"POST", "/api/onboarding/sample"}} {
		if code, _ := e.call(c[0], c[1], "", ""); code != 401 {
			t.Errorf("%s %s without a token = %d, want 401", c[0], c[1], code)
		}
	}
}
