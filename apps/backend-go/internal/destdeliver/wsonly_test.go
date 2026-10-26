package destdeliver

import (
	"context"
	"testing"
	"time"
)

// srcResolver answers every name with a value from a fixed source level.
type srcResolver struct{ source string }

func (s srcResolver) Get(context.Context, string, string) (string, string, error) {
	return "https://hooks.example.test/deployment-level", s.source, nil
}

type countWebhook struct{ n int }

func (c *countWebhook) Send(context.Context, WebhookRequest) (Result, error) {
	c.n++
	return Result{Success: true}, nil
}

// A destination secret reference resolves to workspace secrets only: a global or env value is "not found"
// and nothing is sent.
func TestSlackIgnoresNonWorkspaceSecret(t *testing.T) {
	for _, src := range []string{"global", "default", "api", ""} {
		w := &countWebhook{}
		d := New(Ports{Secrets: srcResolver{src}, Webhook: w}, time.Second)
		r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "slack",
			Config: map[string]any{"webhook_secret_ref": "ANY"}}, 1, nil, "i")
		if r.Success || r.Error != "Slack webhook secret not found" || w.n != 0 {
			t.Errorf("%q: %+v sent=%d", src, r, w.n)
		}
	}
	w := &countWebhook{}
	d := New(Ports{Secrets: srcResolver{"workspace"}, Webhook: w}, time.Second)
	if r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "slack",
		Config: map[string]any{"webhook_secret_ref": "ANY"}}, 1, nil, "i"); !r.Success || w.n != 1 {
		t.Errorf("workspace: %+v", r)
	}
}

// WorkspaceOnly is the getter the warehouse driver uses: a non-workspace value reads as unset.
func TestWorkspaceOnlyGetter(t *testing.T) {
	for src, want := range map[string]string{"workspace": "https://hooks.example.test/deployment-level", "global": "", "default": "", "api": ""} {
		v, err := WorkspaceOnly(srcResolver{src})(context.Background(), "w", "K")
		if err != nil || v != want {
			t.Errorf("%s: %q %v", src, v, err)
		}
	}
	if v, err := WorkspaceOnly(nil)(context.Background(), "w", "K"); v != "" || err != nil {
		t.Errorf("nil resolver: %q %v", v, err)
	}
}
