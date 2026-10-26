package destsync

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

type nopWebhook struct{}

func (nopWebhook) Send(context.Context, destdeliver.WebhookRequest) (destdeliver.Result, error) {
	return destdeliver.Result{}, nil
}

// The worker hands a row back to Python unless the type is in GO_DEST_TYPES and a port
// (or none needed) can deliver it; type text is not normalised beyond what Supports does.
func TestEligibleFollowsPortsAndTypes(t *testing.T) {
	none := destdeliver.Ports{}
	wh := destdeliver.Ports{Webhook: nopWebhook{}}
	cases := []struct {
		typ   string
		ports destdeliver.Ports
		types map[string]bool
		want  bool
	}{
		{"webhook", wh, map[string]bool{"webhook": true}, true},
		{"webhook", none, map[string]bool{"webhook": true}, false}, // port not wired
		{"webhook", wh, map[string]bool{}, false},                  // not in GO_DEST_TYPES
		{"webhook", wh, nil, false},
		{"meta_ads", none, map[string]bool{"meta_ads": true}, true}, // egress client, no port
		{"warehouse_http", none, map[string]bool{"warehouse_http": true}, true},
		{"hubspot", wh, map[string]bool{"hubspot": true}, false},
		{"mystery", wh, map[string]bool{"mystery": true}, false}, // unknown type is never Go's
		{"", wh, map[string]bool{"": true}, false},
	}
	for _, c := range cases {
		if got := Eligible(c.typ, c.ports, c.types); got != c.want {
			t.Errorf("Eligible(%q, types=%v) = %v, want %v", c.typ, c.types, got, c.want)
		}
	}
}

func TestSyncPayloadRequiresWorkspaceAndRun(t *testing.T) {
	const msg = "destination sync requires workspace_id and run_id"
	for _, raw := range []string{``, `null`, `[]`, `{}`, `{"workspace_id":"w"}`, `{"run_id":"r"}`,
		`{"workspace_id":"","run_id":"r"}`, `{"workspace_id":"w","run_id":null}`, `{"workspace_id":5,"run_id":"r"}`} {
		_, _, err := syncPayload([]byte(raw), "")
		if err == nil || err.Error() != msg {
			t.Errorf("payload %q: err = %v, want %q", raw, err, msg)
			continue
		}
		if !queue.IsPermanent(err) {
			t.Errorf("payload %q: a malformed payload cannot be fixed by retrying", raw)
		}
	}
	ws, run, err := syncPayload([]byte(`{"workspace_id":"ws-1","run_id":"run-1","fire_key":"x"}`), "ws-1")
	if err != nil || ws != "ws-1" || run != "run-1" {
		t.Fatalf("valid payload: %q %q %v", ws, run, err)
	}
	if _, _, err := syncPayload([]byte(`{"workspace_id":"ws-1","run_id":"run-1"}`), ""); err != nil {
		t.Fatalf("job row without a workspace column is accepted: %v", err)
	}
}

// The job row's workspace column is the claim's tenant: a payload naming another one is
// refused before anything is read or written.
func TestSyncPayloadWorkspaceMustMatchClaim(t *testing.T) {
	_, _, err := syncPayload([]byte(`{"workspace_id":"ws-victim","run_id":"run-1"}`), "ws-attacker")
	if err == nil || !queue.IsPermanent(err) || !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("mismatch must be a permanent ErrWorkspaceMismatch, got %v", err)
	}
	if _, _, err := failurePayload([]byte(`{"workspace_id":"ws-victim","run_id":"run-1"}`), "ws-attacker"); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("failure payload mismatch = %v", err)
	}
}

// reconcile_destination_job_failure strips both ids and rejects blanks.
func TestFailurePayloadStripsAndRejectsBlank(t *testing.T) {
	const msg = "destination failure payload requires workspace_id and run_id"
	for _, raw := range []string{`{}`, `{"workspace_id":"w"}`, `{"run_id":"r"}`, `{"workspace_id":"  ","run_id":" "}`, `{"workspace_id":"w","run_id":null}`} {
		if _, _, err := failurePayload([]byte(raw), ""); err == nil || err.Error() != msg {
			t.Errorf("payload %q: err = %v, want %q", raw, err, msg)
		}
	}
	ws, run, err := failurePayload([]byte(`{"workspace_id":" ws-vec ","run_id":" run-1 "}`), "ws-vec")
	if err != nil || ws != "ws-vec" || run != "run-1" {
		t.Fatalf("stripped ids: %q %q %v", ws, run, err)
	}
}

// Error text that reaches a ledger row loses credential shapes first and is cut to 1000
// characters afterwards, so a cut can never leave half a secret behind.
func TestErrorTextRedactedBeforeTruncation(t *testing.T) {
	for in, leak := range map[string]string{
		"POST failed: Authorization: Bearer sk_live_abcdef0123456789": "sk_live_abcdef0123456789",
		"request to https://u:hunter2pw@host/x failed":                "hunter2pw",
		"GET /x?access_token=tok_123456&y=1 returned 500":             "tok_123456",
		"upstream said api_key=AKIA0123456789 is invalid":             "AKIA0123456789",
		"Basic dXNlcjpwYXNzd29yZA== rejected":                         "dXNlcjpwYXNzd29yZA==",
	} {
		got := cleanError(in)
		if strings.Contains(got, leak) {
			t.Errorf("cleanError(%q) = %q still holds %q", in, got, leak)
		}
	}
	// the secret straddles the 1000 boundary: redaction first, so no prefix of it survives.
	in := strings.Repeat("x", 990) + " Bearer SECRETSECRETSECRET tail"
	got := cleanError(in)
	if strings.Contains(got, "SECRET") || strings.HasSuffix(got, "Bearer SECR") {
		t.Errorf("boundary secret leaked: ...%q", got[len(got)-30:])
	}
	if n := len([]rune(got)); n > 1000 {
		t.Errorf("length %d > 1000", n)
	}
	if got := cleanError("plain failure"); got != "plain failure" {
		t.Errorf("ordinary text changed: %q", got)
	}
	long := strings.Repeat("é", 1500)
	if n := len([]rune(cleanError(long))); n != 1000 {
		t.Errorf("rune truncation: %d", n)
	}
}
