//go:build integration

package workbook_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// vendorTLS answers every vendor host (the built-in manifests keep their real https URLs; the
// client dials them all to this server). Calls are counted per host.
type vendorTLS struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls map[string]int
}

func newVendorTLS(t *testing.T, h func(host string, body map[string]any) string) *vendorTLS {
	t.Helper()
	v := &vendorTLS{calls: map[string]int{}}
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		v.mu.Lock()
		v.calls[r.Host]++
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, h(r.Host, body))
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *vendorTLS) client() *http.Client {
	addr := v.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server
	}}
}

func (v *vendorTLS) count(host string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls[host]
}

// keySecrets resolves credentials of one workspace only.
type keySecrets struct {
	ws   string
	keys map[string]string
}

func (s keySecrets) Get(_ context.Context, ws, key string) (string, string, error) {
	if ws != s.ws {
		return "", "", nil
	}
	return s.keys[key], "workspace", nil
}

// manifestPayload is what the API planner enqueues: the verified manifests of the chain (from the
// connector index, with their sha256) and the catalog costs.
func manifestPayload(t *testing.T, ws, wb string, idx map[string]manifest.Entry, names ...string) []byte {
	t.Helper()
	manifests, shas, costs := map[string]json.RawMessage{}, map[string]string{}, map[string]float64{}
	for _, n := range names {
		costs[n] = vendorcatalog.BaseCost(n)
		e, ok := idx[n]
		if !ok {
			continue // not declarative: providerrun runs it (or reports it unavailable)
		}
		raw, err := runplan.ManifestJSON(e.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		manifests[n], shas[n] = raw, e.SHA256
	}
	b, _ := json.Marshal(map[string]any{"workspace_id": ws, "workbook_id": wb, "retry_passes": 0, "concurrency": 1,
		"go": map[string]any{"version": 1, "manifests": manifests, "manifest_sha256": shas, "costs": costs, "default_concurrency": 4}})
	return b
}

func (e *env) seedRows(ws, wb, columns string, rows ...string) {
	e.t.Helper()
	e.inWS(ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `INSERT INTO workbooks (id, name, status, workspace_id, source_type, columns_config, budget_max_usd, budget_spent_usd, total_rows, completed_rows)
		  VALUES ($1, 'wb', 'idle', $2, 'manual', $3::json, 10, 0, 0, 0)`, wb, ws, columns); err != nil {
			return err
		}
		for i, data := range rows {
			if _, err := tx.Exec(e.ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments)
			  VALUES ($1, $2, $3, $4::json, '{}'::json)`, wb, ws, i, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// TestDefaultMobileColumnRunsManifestProviders: the default mobile_phone waterfall is
// {leadmagic_mobile, prospeo_mobile}, both built-in declarative manifests with no Go provider.
// Through the production runner (providerrun), the run completes, with spend reserved and
// settled like any paid provider, credentials from the run's workspace only.
func TestDefaultMobileColumnRunsManifestProviders(t *testing.T) {
	e := setup(t, wsA)
	idx := credentials.ConnectorIndex(func(string) (string, bool) { return "", false }, nil)
	vendor := newVendorTLS(t, func(host string, body map[string]any) string {
		switch host {
		case "api.leadmagic.io":
			if body["profile_url"] == "https://www.linkedin.com/in/second" {
				return `{"message":"found","mobile_number":"+15550002"}`
			}
			return `{"message":"not found","mobile_number":null}`
		case "api.prospeo.io":
			return `{"error":false,"response":{"raw_format":"+15550001"}}`
		}
		return `{}`
	})
	e.seedRows(wsA, "wb-mobile", `[{"id":"mob","name":"Mobile","type":"enrichment","target_field":"mobile_phone"}]`,
		`{"company":"Acme","website":"acme-mobile-1.example.com","linkedin_url":"https://www.linkedin.com/in/first"}`,
		`{"company":"Globex","website":"acme-mobile-2.example.com","linkedin_url":"https://www.linkedin.com/in/second"}`)
	sec := keySecrets{ws: wsA, keys: map[string]string{"LEADMAGIC_API_KEY": "lm-key", "PROSPEO_API_KEY": "pr-key"}}
	deps := e.deps(newSim(t, nil), nil)
	deps.Manifests = credentials.NewManifestAllow(credentials.NewLocalManifests(idx, runplan.ManifestJSON))
	deps.Runner = providerrun.New(providerrun.Deps{Pool: e.pool, HTTP: vendor.client(), Secrets: sec, Manifests: idx,
		URLCheck: func(string) error { return nil }})

	c := e.claim(wsA, manifestPayload(t, wsA, "wb-mobile", idx(), "leadmagic_mobile", "prospeo_mobile"))
	res, err := workbook.Run(e.ctx, deps, c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Completed != 2 || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := e.str(wsA, `SELECT string_agg(provider || '=' || value, ',' ORDER BY value) FROM workbook_enrichments WHERE workbook_id='wb-mobile' AND status='complete'`); got != "prospeo_mobile=+15550001,leadmagic_mobile=+15550002" {
		t.Fatalf("cells %q", got)
	}
	if vendor.count("api.leadmagic.io") != 2 || vendor.count("api.prospeo.io") != 1 {
		t.Fatalf("vendor calls leadmagic=%d prospeo=%d", vendor.count("api.leadmagic.io"), vendor.count("api.prospeo.io"))
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE workbook_id='wb-mobile' AND status='settled'`); n != 3 {
		t.Fatalf("settled attempts %d", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE workbook_id='wb-mobile' AND status<>'settled'`); n != 0 {
		t.Fatalf("unsettled attempts %d", n)
	}
	if h := e.one(wsA, `SELECT attempts*10+hits FROM provider_stats WHERE provider='leadmagic_mobile' AND field='mobile_phone'`); h != 21 {
		t.Fatalf("leadmagic_mobile attempts/hits %d", h)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-mobile'`); st != "complete" {
		t.Fatalf("workbook status %s", st)
	}
}

// TestUnknownPaidProviderIsUnavailableAndReleasesSpend: a provider neither Go-native nor
// declarative still fails the run as unavailable, but the call never left the process, so its
// spend attempt is released, never left uncertain, and no budget is held.
func TestUnknownPaidProviderIsUnavailableAndReleasesSpend(t *testing.T) {
	e := setup(t, wsA)
	e.seedRows(wsA, "wb-unknown", `[`+colJSON("c1", "no_such_vendor")+`]`, `{"company":"Acme","website":"acme-unknown.example.com"}`)
	deps := e.deps(newSim(t, nil), nil)
	empty := func() map[string]manifest.Entry { return map[string]manifest.Entry{} }
	deps.Runner = providerrun.New(providerrun.Deps{Pool: e.pool, Secrets: keySecrets{}, Manifests: empty})
	payload := []byte(fmt.Sprintf(`{"workspace_id":%q,"workbook_id":"wb-unknown","retry_passes":0,"concurrency":1,
	  "go":{"version":1,"manifests":{},"manifest_sha256":{},"costs":{"no_such_vendor":0.05},"default_concurrency":4}}`, wsA))
	c := e.claim(wsA, payload)
	_, err := workbook.Run(e.ctx, deps, c)
	if !errors.Is(err, provcall.ErrUnavailable) {
		t.Fatalf("run err %v", err)
	}
	if strings.Contains(err.Error(), "python") {
		t.Fatalf("stale python error text: %v", err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE workbook_id='wb-unknown' AND status IN ('uncertain','dispatched','reserved')`); n != 0 {
		t.Fatalf("held or uncertain attempts %d", n)
	}
	if got := e.spentUSD(wsA, "wb-unknown"); got != 0 {
		t.Fatalf("budget_spent_usd %v", got)
	}
}
