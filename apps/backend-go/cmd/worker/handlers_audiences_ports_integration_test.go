//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// okVendor answers every vendor request with one body each client reads as a success (no contact/lead found,
// record created, row appended, lead added) and counts the requests.
type okVendor struct{ n atomic.Int32 }

func (v *okVendor) RoundTrip(r *http.Request) (*http.Response, error) {
	v.n.Add(1)
	body := `{"id":"x1","success":true,"total":0,"results":[],"totalSize":0,"records":[{"id":"rec1"}],"createdRecords":["rec1"],` +
		`"values":[],"updates":{"updatedRange":"Sheet1!A2:B2"},"upload_count":1,"duplicate_count":0}`
	if strings.HasSuffix(r.URL.Host, ".salesforce.com") { // query miss, then Lead created
		body = `{"totalSize":0,"records":[]}`
		if r.Method != http.MethodGet {
			body = `{"id":"00Q1","success":true}`
		}
	}
	status := 201 // created (CRM, campaigns)
	if r.Method == http.MethodGet || r.URL.Host == "sheets.googleapis.com" {
		status = 200 // lookups and Sheets writes answer 200
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

type wsStore map[string]string

func (s wsStore) AppSetting(context.Context, string) (string, error) { return "", nil }
func (s wsStore) WorkspaceSetting(_ context.Context, _, k string) (string, bool, error) {
	v, ok := s[k]
	return v, ok, nil
}

// Plan 15 gap closed: every plan-07 destination type runs its audience_destination_sync job to completion in the Go
// worker with the production port wiring (destPorts), credentials from the workspace only.
func TestWorkerSyncsPlan07DestinationTypes(t *testing.T) {
	cfgs := map[string]string{
		"hubspot": `{}`, "salesforce": `{}`, "instantly": `{"campaign_id":"c1"}`, "smartlead": `{"campaign_id":"7"}`,
		"google_sheets": `{"spreadsheet_id":"s1","columns":["email"]}`, "airtable": `{"base_id":"appB","table":"Leads"}`,
	}
	sec := secrets.NewResolver(nil, wsStore{
		"HUBSPOT_TOKEN": "h", "SALESFORCE_INSTANCE_URL": "https://acme.my.salesforce.com", "SALESFORCE_ACCESS_TOKEN": "s",
		"INSTANTLY_API_KEY": "i", "SMARTLEAD_API_KEY": "l", "GOOGLE_SHEETS_TOKEN": "g", "AIRTABLE_TOKEN": "a",
	}, func(string) string { return "" })
	for typ, cfg := range cfgs {
		t.Run(typ, func(t *testing.T) {
			g := setupAud(t)
			run, mk := g.destRun(t, typ, 2)
			g.exec(t, `UPDATE audience_destinations SET config=$2::json WHERE id='d1' AND workspace_id=$1`, g.ws, cfg)
			g.exec(t, `UPDATE audience_members SET snapshot=json_build_object('id', lead_id, 'email', 'u'||lead_id||'@x.test', 'company', 'Co') WHERE workspace_id=$1`, g.ws)
			job := mk(0, "go")
			v := &okVendor{}
			g.runAudSec(t, audConfig(g.ws), &nopEmitter{}, audienceOpts{http: &http.Client{Transport: v}}, sec)
			if st, _, _ := g.jobRow(t, job); st != "completed" {
				t.Fatalf("job %s", st)
			}
			if s, e := g.runState(t, run); s != "completed" {
				var errs []string
				rows, _ := g.owner.Query(context.Background(), `SELECT coalesce(error,'') FROM destination_deliveries WHERE run_id=$1`, run)
				for rows.Next() {
					var x string
					_ = rows.Scan(&x)
					errs = append(errs, x)
				}
				rows.Close()
				t.Fatalf("run %s %q deliveries %q", s, e, errs)
			}
			if v.n.Load() < 2 {
				t.Fatalf("%d vendor requests", v.n.Load())
			}
		})
	}
}
