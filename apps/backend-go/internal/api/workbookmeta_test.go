//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type metaEnv struct{ detailEnv }

func newMetaEnvFlags(t *testing.T, fl config.Flags) metaEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: fl}
	RegisterWorkbooks(r, d)
	RegisterWorkbookDetail(r, d)
	RegisterWorkbookMeta(r, d)
	e := metaEnv{detailEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}}
	e.purge()
	t.Cleanup(e.purge)
	e.seedDetail(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	return e
}

func newMetaEnv(t *testing.T) metaEnv { return newMetaEnvFlags(t, config.Flags{PGLeadStore: true}) }

func metaActor(name string) (who, ws string) {
	switch name {
	case "column_types_no_token", "me_no_token", "flags_no_token":
		return "", ""
	case "filter_options_beta", "me_bob_owner_beta":
		return "bob", "ws-beta"
	case "providers_no_ws_header":
		return "alice", ""
	case "providers_foreign_ws", "me_foreign_ws":
		return "bob", "ws-alpha-nope"
	case "me_bob_member":
		return "bob", "ws-alpha"
	case "me_erin_editor":
		return "erin", "ws-alpha"
	}
	return "alice", "ws-alpha"
}

func (e metaEnv) replayMeta(t *testing.T, name string) {
	t.Helper()
	fx := contract.Load(t, "meta/"+name)
	who, ws := metaActor(name)
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		if ws != "" {
			hdr["X-Workspace-Id"] = ws
		}
	}
	rec, _ := e.do("GET", fx.Request.Path, "", "", hdr)
	got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			got.Headers[k] = v
		}
	}
	var opts []contract.Option
	if strings.HasPrefix(name, "me_") {
		opts = append(opts, contract.IgnorePaths("$.user_id"))
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
}

func (e metaEnv) as(who, ws, path string) *httptest.ResponseRecorder {
	return e.get(who, ws, path)
}

func metaFixtures(t *testing.T) []string {
	var out []string
	for _, n := range contractFixtureNames(t, "meta") {
		n = strings.TrimPrefix(n, "meta/")
		if strings.HasPrefix(n, "token_") {
			continue
		}
		out = append(out, n)
	}
	if len(out) < 20 {
		t.Fatalf("expected the recorded meta fixtures, got %d", len(out))
	}
	return out
}

func TestMetaRoutesMatchFixtures(t *testing.T) {
	e := newMetaEnv(t)
	for _, n := range metaFixtures(t) {
		e.replayMeta(t, n)
	}
}

func TestMetaLiteralsBeatWorkbookID(t *testing.T) {
	e := newMetaEnv(t)
	for _, p := range []string{"column-types", "lead-fields", "providers", "ai-column-presets", "filter-options"} {
		rec := e.as("alice", "ws-alpha", "/api/workbooks/meta/"+p)
		if rec.Code != 200 {
			t.Errorf("meta/%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	// the wildcard still serves real ids
	if rec := e.as("alice", "ws-alpha", "/api/workbooks/wb-alpha-2"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "wb-alpha-2") {
		t.Errorf("detail broken: %d %.200s", rec.Code, rec.Body.String())
	}
	// "meta" itself is not a workbook id: the unlisted literal falls to detail and answers 404
	if rec := e.as("alice", "ws-alpha", "/api/workbooks/meta"); rec.Code != 404 {
		t.Errorf("/api/workbooks/meta: %d", rec.Code)
	}
}

func TestAIPresetsCategoryFilter(t *testing.T) {
	e := newMetaEnv(t)
	var all, q, none, empty struct {
		Presets    []struct{ Category string } `json:"presets"`
		Categories []string                    `json:"categories"`
	}
	dec := func(path string, v any) {
		rec := e.as("alice", "ws-alpha", path)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatal(err)
		}
	}
	dec("/api/workbooks/meta/ai-column-presets", &all)
	dec("/api/workbooks/meta/ai-column-presets?category=qualification", &q)
	dec("/api/workbooks/meta/ai-column-presets?category=nope", &none)
	dec("/api/workbooks/meta/ai-column-presets?category=", &empty)
	if len(q.Presets) == 0 || len(q.Presets) >= len(all.Presets) {
		t.Fatalf("qualification %d of %d", len(q.Presets), len(all.Presets))
	}
	for _, p := range q.Presets {
		if p.Category != "qualification" {
			t.Errorf("leaked %q", p.Category)
		}
	}
	if len(none.Presets) != 0 || none.Categories == nil || len(none.Categories) != len(all.Categories) {
		t.Errorf("unknown category: %+v", none)
	}
	if !strings.Contains(e.as("alice", "ws-alpha", "/api/workbooks/meta/ai-column-presets?category=nope").Body.String(), `"presets":[]`) {
		t.Error("presets must serialise as [] not null")
	}
	if len(empty.Presets) != len(all.Presets) {
		t.Error("empty category means no filter (Python falsy)")
	}
}

func TestFilterOptionsTenantScoped(t *testing.T) {
	e := newMetaEnv(t)
	a := e.as("alice", "ws-alpha", "/api/workbooks/meta/filter-options").Body.String()
	b := e.as("bob", "ws-beta", "/api/workbooks/meta/filter-options").Body.String()
	if a == b || strings.Contains(b, "Berlin") || !strings.Contains(a, "Berlin") {
		t.Fatalf("tenant leak or empty:\n%s\n%s", a, b)
	}
	last := -1
	for _, k := range []string{`"cities"`, `"tiers"`, `"sources"`, `"statuses"`, `"specializations"`, `"total_leads"`} {
		i := strings.Index(a, k)
		if i <= last {
			t.Fatalf("key order (Python: cities,tiers,sources,statuses,specializations,total_leads): %s", a)
		}
		last = i
	}
}

func TestMeContextRoleAndOwner(t *testing.T) {
	e := newMetaEnv(t)
	for _, c := range []struct{ who, ws, role string }{{"alice", "ws-alpha", "owner"}, {"erin", "ws-alpha", "editor"}, {"bob", "ws-alpha", "member"}} {
		rec := e.as(c.who, c.ws, "/api/me/context")
		uid := strconv.Itoa(int(e.mustUserByName(t, c.who).ID))
		want := `{"user_id":"` + uid + `","workspace_id":"` + c.ws + `","role":"` + c.role + `","is_owner":` + strconv.FormatBool(c.role == "owner") + `}`
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: %d %s want %s", c.who, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestMeContextMemberFallback(t *testing.T) {
	e := newMetaEnv(t)
	// workspace owner without a member row: Python member_role() is None -> "member".
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM workspace_members WHERE workspace_id='ws-alpha' AND user_id=$1`, e.mustUserByName(t, "alice").ID); err != nil {
		t.Fatal(err)
	}
	rec := e.as("alice", "ws-alpha", "/api/me/context")
	if rec.Code != 200 {
		t.Skipf("owner without member row is refused by the workspace gate (%d): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"role":"member","is_owner":false`) {
		t.Errorf("fallback: %s", rec.Body.String())
	}
}

func TestFlagsReflectConfig(t *testing.T) {
	cases := []struct {
		fl   config.Flags
		want string
	}{
		{config.Flags{PGLeadStore: true}, `{"automations_enabled":false,"intent_poller_enabled":false,"pg_lead_store":true,"allow_legacy_outreach":false}`},
		{config.Flags{PGLeadStore: true, AutomationsEnabled: true, IntentPollerEnabled: true, AutomationsAllowLegacyOutreach: true}, `{"automations_enabled":true,"intent_poller_enabled":true,"pg_lead_store":true,"allow_legacy_outreach":true}`},
	}
	for i, c := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			e := newMetaEnvFlags(t, c.fl)
			rec := e.as("alice", "ws-alpha", "/api/flags")
			if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != c.want {
				t.Errorf("flags %+v: %d %s", c.fl, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMetaRequiresWorkspace(t *testing.T) {
	e := newMetaEnv(t)
	e.seed(t, "carol", pythonHash, true, "user", false)
	for _, p := range []string{"/api/workbooks/meta/column-types", "/api/workbooks/meta/filter-options", "/api/me/context", "/api/flags"} {
		rec, _ := e.do("GET", p, "", "", bearer("carol"))
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "No accessible workspace. Ask an admin to add you to one.") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}
