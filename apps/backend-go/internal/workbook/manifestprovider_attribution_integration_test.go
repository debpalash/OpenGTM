//go:build integration

package workbook_test

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// Issue 114 closure (declarative provider attribution): the original defect was a declarative result that omitted
// provider=self.name. A vendor that answers with its own "provider" / "src" fields must not be able to rename the
// outer result either. Traced through the production runner (providerrun -> rpclocal-shaped provider name), the
// waterfall cell, the persisted workbook_enrichments evidence and the row cell the workbook UI renders.
func TestDeclarativeProviderAttributionSurvivesWaterfallEvidenceAndRowPresentation(t *testing.T) {
	e := setup(t, wsA)
	idx := credentials.ConnectorIndex(func(string) (string, bool) { return "", false }, nil)
	vendor := newVendorTLS(t, func(host string, _ map[string]any) string {
		if host == "api.leadmagic.io" {
			return `{"message":"found","mobile_number":"+15550009","provider":"evil_vendor","src":"evil_vendor"}`
		}
		return `{}`
	})
	const mobileCol = `[{"id":"mob","name":"Mobile","type":"enrichment","target_field":"mobile_phone"}]`
	e.seedRows(wsA, "wb-attr", mobileCol, `{"company":"Acme","website":"acme-attr.example.com","linkedin_url":"https://www.linkedin.com/in/first"}`)
	deps := e.deps(newSim(t, nil), nil)
	deps.Manifests = credentials.NewManifestAllow(credentials.NewLocalManifests(idx, runplan.ManifestJSON))
	deps.Runner = providerrun.New(providerrun.Deps{Pool: e.pool, HTTP: vendor.client(), Secrets: keySecrets{ws: wsA, keys: map[string]string{"LEADMAGIC_API_KEY": "lm-key"}},
		Manifests: idx, URLCheck: func(string) error { return nil }})
	res, err := workbook.Run(e.ctx, deps, e.claim(wsA, manifestPayload(t, wsA, "wb-attr", idx(), "leadmagic_mobile")))
	if err != nil || res.Completed != 1 || res.Errors != 0 {
		t.Fatalf("run %+v %v", res, err)
	}
	if vendor.count("api.leadmagic.io") != 1 {
		t.Fatalf("vendor calls %d", vendor.count("api.leadmagic.io"))
	}

	// Persisted evidence.
	if got := e.str(wsA, `SELECT provider || '=' || value || ':' || status FROM workbook_enrichments WHERE workbook_id='wb-attr' AND column_id='mob'`); got != "leadmagic_mobile=+15550009:complete" {
		t.Fatalf("evidence row %q", got)
	}
	// Row presentation: the cell the workbook renders carries the same provider, never the vendor's claim.
	cell := e.str(wsA, `SELECT enrichments::jsonb -> 'mob' ->> 'provider' || '|' || (enrichments::jsonb -> 'mob' ->> 'value') || '|' || (enrichments::jsonb -> 'mob' ->> 'status') FROM workbook_rows WHERE workbook_id='wb-attr'`)
	if cell != "leadmagic_mobile|+15550009|complete" {
		t.Fatalf("row cell %q", cell)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE workbook_id='wb-attr' AND provider = 'evil_vendor'`); n != 0 {
		t.Fatalf("vendor-claimed provider persisted (%d rows)", n)
	}
}
