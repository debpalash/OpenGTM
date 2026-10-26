//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcecatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcehealth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// ssEnv is the settings-sources route group over a seeded global source_health table and the SOURCE_*_ENABLED
// overrides of legacy/python/scripts/contract_seed.py (LG_SOURCE_HEALTH, LG_SETTINGS).
type ssEnv struct{ mcpEnv }

const srcSeedStamp = "2026-09-01T12:00:00" // the recorder pins source_health.updated_at of rows written during the run

func newSSEnv(t *testing.T, mut ...func(*Deps)) ssEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Envelope: envl, CollectionLedgerBackend: "postgres",
		Getenv: func(string) string { return "" },
		Flags:  config.Flags{PGLeadStore: true, Leadgen: config.LeadgenSettings{GoCollectionRoutesEnabled: true}}}
	for _, m := range mut {
		m(&d)
	}
	RegisterSourceSettings(r, d)
	e := ssEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.reseed(t)
	t.Cleanup(func() {
		e.wipe(t)
		srcHealthSummary, srcHealthMap, srcManualEnable, srcHealthReset = nil, nil, nil, nil
	})
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	return e
}

var srcSettingKeys = []string{"SOURCE_GOOGLE_MAPS_ENABLED", "SOURCE_LINKEDIN_ENABLED", "SOURCE_ZAUBACORP_ENABLED", "SOURCE_JUSTDIAL_ENABLED",
	"SOURCE_INDIAMART_ENABLED", "SOURCE_TRADEINDIA_ENABLED", "SOURCE_DUCKDUCKGO_ENABLED", "SOURCE_SULEKHA_ENABLED", "SOURCE_NOT_A_SOURCE_ENABLED"}

func (e ssEnv) wipe(t *testing.T) {
	t.Helper()
	ownerExec(t, `DELETE FROM source_health`)
	ownerExec(t, `DELETE FROM app_settings WHERE key = ANY($1)`, srcSettingKeys)
}

func (e ssEnv) reseed(t *testing.T) {
	t.Helper()
	e.wipe(t)
	const ins = `INSERT INTO source_health (name, state, consecutive_zero, consecutive_nonzero, last_yield, ewma_yield, probes,
	 last_probe_at, last_ok_at, last_outage_at, disabled_at, disabled_reason, manual_override, updated_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8::timestamp,$9::timestamp,$10::timestamp,$11::timestamp,$12,$13,$14::timestamp)`
	const d = "2026-08-29 03:00:00"
	for _, r := range [][]any{
		{"indiamart", "healthy", 0, 4, 6, 5.25, 12, d, d, nil, nil, nil, false, d},
		{"justdial", "degraded", 3, 0, 0, 0.45, 9, d, "2026-08-26 03:00:00", "2026-08-28 03:00:00", nil, nil, false, d},
		{"tradeindia", "auto_disabled", 7, 0, 0, 0.0, 14, d, "2026-08-21 03:00:00", nil, "2026-08-27 03:00:00", "zero_yield", false, "2026-08-27 03:00:00"},
		{"sulekha", "healthy", 0, 2, 3, 2.6666666, 8, d, d, nil, nil, nil, true, "2026-08-28 03:00:00"},
		{"exportersindia", "degraded", 4, 0, 0, 1.0, 10, d, "2026-08-25 03:00:00", nil, nil, nil, true, d},
		{"clutch", "auto_disabled", 6, 0, 0, 0.1234, 11, d, "2026-08-20 03:00:00", "2026-08-27 03:00:00", "2026-08-26 03:00:00", "zero_yield", true, "2026-08-26 03:00:00"},
		{"goodfirms", "healthy", 0, 1, 1, 1.0, 1, nil, nil, nil, nil, nil, false, nil},
	} {
		ownerExec(t, ins, r...)
	}
	for _, kv := range [][2]string{{"SOURCE_GOOGLE_MAPS_ENABLED", "0"}, {"SOURCE_LINKEDIN_ENABLED", ""}, {"SOURCE_ZAUBACORP_ENABLED", "1"}, {"SOURCE_JUSTDIAL_ENABLED", "yes"}} {
		ownerExec(t, `INSERT INTO app_settings (key, value) VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET value = $2`, kv[0], kv[1])
	}
}

// ownerJSON runs a single-value query as the database owner and returns the text (nil-safe).
func ownerJSON(t *testing.T, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var v *string
	if err := c.QueryRow(ctx, sql, args...).Scan(&v); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func healthRow(t *testing.T, name string) string {
	t.Helper()
	return ownerJSON(t, `SELECT to_jsonb(s)::text FROM source_health s WHERE name = $1`, name)
}

func settingOf(t *testing.T, key string) string {
	t.Helper()
	return ownerJSON(t, `SELECT value FROM app_settings WHERE key = $1`, key)
}

func (e ssEnv) ssCall(method, path, who string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	rec, _ := e.do(method, path, "", "", hdr)
	return rec
}

// srcNormalise pins the volatile source_health.updated_at stamps: rows written during the run carry the recorder's
// pinned instant, ours carry the test clock; seeded stamps (2026-08-*) are left alone.
func srcNormalise(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var walk func(any)
	walk = func(x any) {
		switch n := x.(type) {
		case map[string]any:
			for k, c := range n {
				if s, ok := c.(string); ok && k == "updated_at" && !strings.HasPrefix(s, "2026-08") {
					n[k] = srcSeedStamp
				}
				walk(c)
			}
		case []any:
			for _, c := range n {
				walk(c)
			}
		}
	}
	walk(v)
	b, _ := json.Marshal(v)
	return b
}

// srcFixtureOrder is the recorded case order (state carries from one case to the next).
var srcFixtureOrder = []string{
	"list_admin", "list_non_admin", "list_no_auth", "health_admin", "health_non_admin", "health_no_auth",
	"registry_admin", "registry_non_admin", "registry_no_auth",
	"put_non_admin_403", "put_no_auth_401", "put_unknown_404", "put_enabled_bad_422", "put_reenable_google_maps",
	"put_default_param_true", "put_disable_indiamart", "put_enable_auto_disabled", "put_enable_no_health_row",
	"put_enable_non_registry_source", "put_disable_default_off_source", "put_enabled_numeric_one",
	"list_after_put", "health_after_put",
	"reset_non_admin_403", "reset_no_auth_401", "reset_degraded", "reset_manual_override", "reset_auto_disabled_override",
	"reset_no_row", "reset_unknown_source", "health_after_reset", "list_final",
}

func TestSourcesMatchFixtures(t *testing.T) {
	e := newSSEnv(t)
	onDisk := map[string]bool{}
	for _, full := range contractFixtureNames(t, "sources") {
		if n := strings.TrimPrefix(full, "sources/srcsettings_"); n != strings.TrimPrefix(full, "sources/") {
			onDisk[n] = true
		}
	}
	if len(onDisk) != len(srcFixtureOrder) {
		t.Fatalf("%d srcsettings fixtures on disk, %d in the replay order", len(onDisk), len(srcFixtureOrder))
	}
	for _, n := range srcFixtureOrder {
		if !onDisk[n] {
			t.Fatalf("fixture srcsettings_%s missing", n)
		}
		fx := contract.Load(t, "sources/srcsettings_"+n)
		who := "alice"
		switch {
		case strings.Contains(n, "no_auth"):
			who = ""
		case strings.Contains(n, "non_admin"):
			who = "bob"
		}
		rec := e.ssCall(fx.Request.Method, fx.Request.Path, who)
		got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: srcNormalise(t, rec.Body.Bytes())}
		for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
			if v := rec.Header().Get(k); v != "" {
				got.Headers[k] = v
			}
		}
		want := fx.Response
		want.Body = srcNormalise(t, want.Body)
		t.Run(n, func(t *testing.T) { contract.AssertEqual(t, got, want) })
	}
}

func TestSourcesHealthBadgeNullWithoutRow(t *testing.T) {
	e := newSSEnv(t)
	ownerExec(t, `DELETE FROM source_health`)
	rec := e.ssCall("GET", "/api/settings/sources", "bob")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != len(sourcecatalog.Sources()) {
		t.Fatalf("%v, %d entries", err, len(list))
	}
	for _, s := range list {
		if s["health"] != nil {
			t.Fatalf("%v: health %v, want null", s["id"], s["health"])
		}
	}
	// With a row the badge has exactly the five Python keys, in order.
	ownerExec(t, `INSERT INTO source_health (name, state, last_yield, manual_override) VALUES ('duckduckgo','healthy',4,true)`)
	body := e.ssCall("GET", "/api/settings/sources", "bob").Body.String()
	if !strings.Contains(body, `"health":{"state":"healthy","last_yield":4,"last_ok_at":null,"disabled_reason":null,"manual_override":true}`) {
		t.Fatalf("badge: %s", body)
	}
}

func TestSourcesHealthErrorShape(t *testing.T) {
	e := newSSEnv(t)
	boom := errors.New("secret detail: password=hunter2")
	srcHealthSummary = func(context.Context, pgx.Tx, sourcehealth.Flags) (*sourcehealth.Obj, error) { return nil, boom }
	rec := e.ssCall("GET", "/api/settings/sources/health", "bob")
	if rec.Code != 200 || rec.Body.String() != `{"total":0,"sources":[],"error":"internal error"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// A health-layer error on the list degrades to null badges, status 200.
	srcHealthMap = func(context.Context, pgx.Tx) (map[string]*sourcehealth.Obj, error) { return nil, boom }
	rec = e.ssCall("GET", "/api/settings/sources", "bob")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"health":{`) || !strings.Contains(rec.Body.String(), `"health":null`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// manual_enable errors are swallowed: the toggle still succeeds and is stored.
	srcManualEnable = func(context.Context, pgx.Tx, string) (*sourcehealth.Obj, error) { return nil, boom }
	rec = e.ssCall("PUT", "/api/settings/sources/linkedin?enabled=true", "alice")
	if rec.Code != 200 || rec.Body.String() != `{"id":"linkedin","enabled":true}` || settingOf(t, "SOURCE_LINKEDIN_ENABLED") != "1" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// reset_health errors are a 500 with a generic detail (never the error text).
	srcHealthReset = func(context.Context, pgx.Tx, string) (*sourcehealth.Obj, error) { return nil, boom }
	rec = e.ssCall("POST", "/api/settings/sources/justdial/health/reset", "alice")
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestPutUnknownSource404(t *testing.T) {
	e := newSSEnv(t)
	rec := e.ssCall("PUT", "/api/settings/sources/not_a_source?enabled=true", "alice")
	if rec.Code != 404 || rec.Body.String() != `{"detail":"Source 'not_a_source' not found"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if v := settingOf(t, "SOURCE_NOT_A_SOURCE_ENABLED"); v != "" {
		t.Fatalf("setting written for an unknown source: %q", v)
	}
	if healthRow(t, "not_a_source") != "" {
		t.Fatal("health row created for an unknown source")
	}
}

func TestPutNonAdmin403(t *testing.T) {
	e := newSSEnv(t)
	before := healthRow(t, "indiamart")
	for _, p := range []string{"PUT /api/settings/sources/google_maps?enabled=true", "PUT /api/settings/sources/indiamart?enabled=false",
		"PUT /api/settings/sources/nope?enabled=maybe", "POST /api/settings/sources/indiamart/health/reset"} {
		m, path, _ := strings.Cut(p, " ")
		rec := e.ssCall(m, path, "bob")
		if rec.Code != 403 || rec.Body.String() != `{"detail":"Not authorized"}` {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
	if settingOf(t, "SOURCE_GOOGLE_MAPS_ENABLED") != "0" || settingOf(t, "SOURCE_INDIAMART_ENABLED") != "" || healthRow(t, "indiamart") != before {
		t.Fatal("a rejected call changed state")
	}
	if rec := e.ssCall("PUT", "/api/settings/sources/google_maps?enabled=true", ""); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
}

func TestPutEnableClearsAutoDisabled(t *testing.T) {
	e := newSSEnv(t)
	rec := e.ssCall("PUT", "/api/settings/sources/tradeindia?enabled=true", "alice")
	if rec.Code != 200 || rec.Body.String() != `{"id":"tradeindia","enabled":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(healthRow(t, "tradeindia")), &row); err != nil {
		t.Fatal(err)
	}
	if row["state"] == "auto_disabled" || row["manual_override"] != true || row["consecutive_zero"] != float64(0) || row["disabled_reason"] != nil {
		t.Fatalf("health row after enable: %v", row)
	}
	if settingOf(t, "SOURCE_TRADEINDIA_ENABLED") != "1" {
		t.Fatal("setting not stored as 1")
	}
	// No row yet: enabling creates a pinned one.
	if rec := e.ssCall("PUT", "/api/settings/sources/duckduckgo?enabled=true", "alice"); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if !strings.Contains(healthRow(t, "duckduckgo"), `"manual_override": true`) {
		t.Fatalf("no pinned row: %s", healthRow(t, "duckduckgo"))
	}
}

func TestPutDisableDoesNotTouchHealth(t *testing.T) {
	e := newSSEnv(t)
	before := healthRow(t, "indiamart")
	rec := e.ssCall("PUT", "/api/settings/sources/indiamart?enabled=false", "alice")
	if rec.Code != 200 || rec.Body.String() != `{"id":"indiamart","enabled":false}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if healthRow(t, "indiamart") != before {
		t.Fatal("disable changed the health row")
	}
	if settingOf(t, "SOURCE_INDIAMART_ENABLED") != "0" {
		t.Fatal("setting not stored as 0")
	}
	// Disabling a source with no row must not create one.
	if rec := e.ssCall("PUT", "/api/settings/sources/zaubacorp?enabled=false", "alice"); rec.Code != 200 || healthRow(t, "zaubacorp") != "" {
		t.Fatalf("%d row=%q", rec.Code, healthRow(t, "zaubacorp"))
	}
}

func TestResetNoRowShape(t *testing.T) {
	e := newSSEnv(t)
	rec := e.ssCall("POST", "/api/settings/sources/bbb/health/reset", "alice")
	if rec.Code != 200 || rec.Body.String() != `{"name":"bbb","reset":false,"detail":"no health row"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = e.ssCall("POST", "/api/settings/sources/justdial/health/reset", "alice")
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), `{"name":"justdial","state":"healthy"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestRegistryMatchesPython(t *testing.T) {
	e := newSSEnv(t)
	rec := e.ssCall("GET", "/api/settings/sources/registry", "bob")
	var want bytes.Buffer
	enc := json.NewEncoder(&want)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sourcecatalog.Registry()); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Body.String() != strings.TrimSuffix(want.String(), "\n") {
		t.Fatalf("registry differs from the catalog dump (%d)", rec.Code)
	}
}

func TestEnabledQueryParamDefaultTrue(t *testing.T) {
	e := newSSEnv(t)
	for q, want := range map[string]string{"": "1", "?enabled=true": "1", "?enabled=ON": "1", "?enabled=1": "1", "?enabled=No": "0", "?enabled=0": "0",
		"?enabled=off&enabled=yes": "1"} {
		rec := e.ssCall("PUT", "/api/settings/sources/linkedin"+q, "alice")
		if rec.Code != 200 || settingOf(t, "SOURCE_LINKEDIN_ENABLED") != want {
			t.Fatalf("%q: %d %s stored %q, want %q", q, rec.Code, rec.Body, settingOf(t, "SOURCE_LINKEDIN_ENABLED"), want)
		}
	}
	rec := e.ssCall("PUT", "/api/settings/sources/linkedin?enabled=", "alice")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"type":"bool_parsing"`) {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
}

func TestSourcesRouteOrdering(t *testing.T) {
	e := newSSEnv(t)
	if rec := e.ssCall("GET", "/api/settings/sources/health", "bob"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"by_state"`) {
		t.Fatalf("health captured by {source_id}: %d %s", rec.Code, rec.Body)
	}
	if rec := e.ssCall("GET", "/api/settings/sources/registry", "bob"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"by_region"`) {
		t.Fatalf("registry captured by {source_id}: %d %s", rec.Code, rec.Body)
	}
	// Only PUT is a {source_id} route: the other literal names fall to it as ids and are unknown sources.
	for _, id := range []string{"health", "registry"} {
		rec := e.ssCall("PUT", "/api/settings/sources/"+id, "alice")
		if rec.Code != 404 || rec.Body.String() != `{"detail":"Source '`+id+`' not found"}` {
			t.Fatalf("PUT %s: %d %s", id, rec.Code, rec.Body)
		}
	}
}
