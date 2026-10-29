package scrape

import (
	"context"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const liAtSecret = "AQEDAR-workspace-li-at-SECRET-0123456789"

// echoStaff is a browser service that echoes request data back (the worst case for a leak).
type echoStaff struct {
	renderFake
	got   []browserclient.StaffSpyRequest
	err   string
	staff []browserclient.StaffRow
}

func (e *echoStaff) StaffSpy(_ context.Context, r browserclient.StaffSpyRequest) (browserclient.StaffSpyResult, error) {
	e.got = append(e.got, r)
	return browserclient.StaffSpyResult{Error: e.err, Staff: e.staff}, nil
}

func wsCookies(seen *[]string) func(ctx context.Context, ws string) ([]browserclient.Cookie, error) {
	return func(_ context.Context, ws string) ([]browserclient.Cookie, error) {
		*seen = append(*seen, ws)
		if ws != "ws-a" {
			return nil, nil
		}
		return []browserclient.Cookie{{Name: "li_at", Value: liAtSecret, Domain: ".linkedin.com"}}, nil
	}
}

// L7: the LinkedIn session is the calling workspace's own secret; no workspace -> unavailable.
func TestStaffSpySessionIsPerWorkspace(t *testing.T) {
	t.Cleanup(func() { Configure(Deps{}) })
	var seen []string
	br := &echoStaff{staff: []browserclient.StaffRow{{Name: "Jane Doe", Title: "CEO"}}}
	Configure(Deps{Browser: br, StaffSpyEnabled: true, LinkedInCookies: wsCookies(&seen)})
	in := providers.Input{Lead: map[string]any{"company": "Acme"}}

	out, err := staffSpy{}.Enrich(context.Background(), nil, nil, in)
	if err != nil || out.Error != staffspyUnavailable || len(br.got) != 0 {
		t.Fatalf("no workspace on ctx: %+v %v (requests %d)", out, err, len(br.got))
	}
	out, _ = staffSpy{}.Enrich(providers.WithWorkspace(context.Background(), "ws-b"), nil, nil, in)
	if out.Error != staffspyUnavailable || len(br.got) != 0 {
		t.Fatalf("workspace without a session: %+v", out)
	}
	out, _ = staffSpy{}.Enrich(providers.WithWorkspace(context.Background(), "ws-a"), nil, nil, in)
	if out.Error != "" || len(br.got) != 1 || br.got[0].Cookies[0].Value != liAtSecret {
		t.Fatalf("workspace session: %+v", out)
	}
	if strings.Join(seen, ",") != "ws-b,ws-a" {
		t.Fatalf("session lookups %v", seen)
	}
}

// L7: whatever the service echoes, the session never reaches the provider output.
func TestStaffSpyScrubsSession(t *testing.T) {
	t.Cleanup(func() { Configure(Deps{}) })
	var seen []string
	br := &echoStaff{err: "login failed for cookie li_at=" + liAtSecret}
	Configure(Deps{Browser: br, StaffSpyEnabled: true, LinkedInCookies: wsCookies(&seen)})
	ctx := providers.WithWorkspace(context.Background(), "ws-a")
	in := providers.Input{Lead: map[string]any{"company": "Acme"}}
	out, _ := staffSpy{}.Enrich(ctx, nil, nil, in)
	if strings.Contains(out.Error, liAtSecret) || out.Error != "staffspy failed: failed" {
		t.Fatalf("free-text service error passed through: %q", out.Error)
	}
	for _, code := range []string{"login_failed", "rate_limited", "failed"} {
		br.err = code
		got, _ := staffSpy{}.Enrich(ctx, nil, nil, in)
		if got.Error != "staffspy failed: "+code {
			t.Errorf("code %s -> %q", code, got.Error)
		}
	}
	br.err = ""
	br.staff = []browserclient.StaffRow{{Name: "Jane " + liAtSecret, Title: "CEO", LinkedIn: "https://x/" + liAtSecret}}
	out, _ = staffSpy{}.Enrich(ctx, nil, nil, in)
	raw, _ := out.Extra.MarshalJSON()
	if strings.Contains(string(raw), liAtSecret) {
		t.Fatalf("session echoed into rows: %s", raw)
	}
}
