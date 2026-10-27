package workbook

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

type memStore struct{ app, ws map[string]string }

func (s *memStore) AppSetting(_ context.Context, k string) (string, error) { return s.app[k], nil }
func (s *memStore) WorkspaceSetting(_ context.Context, ws, k string) (string, bool, error) {
	v, ok := s.ws[ws+"/"+k]
	return v, ok, nil
}
func (s *memStore) SetWorkspaceSetting(_ context.Context, ws, k, v string) error {
	s.ws[ws+"/"+k] = v
	return nil
}

type hubspotFake struct{ pushed int }

func (h *hubspotFake) IsConnected(context.Context, string) bool { return true }
func (h *hubspotFake) UpdateContactByID(context.Context, string, string, crm.FieldMap) crm.UpdateResult {
	return crm.UpdateResult{}
}
func (h *hubspotFake) PushLeadAsContact(context.Context, string, crm.Lead, crm.FieldMap) crm.PushResult {
	h.pushed++
	return crm.PushResult{}
}

// L6: output destinations use the workspace's own credentials, never global settings or env.
func TestOutputDestinationsResolveWorkspaceCredentialsOnly(t *testing.T) {
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: "k", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := &memStore{app: map[string]string{"AIRTABLE_API_KEY": "global-key", "HUBSPOT_TOKEN": "global-token"}, ws: map[string]string{}}
	r := secrets.NewResolver(e, st, func(k string) string {
		if k == "SMARTLEAD_API_KEY" {
			return "env-key"
		}
		return ""
	})
	ctx := context.Background()
	ws := workspaceSecrets{r: r}
	for _, k := range []string{"AIRTABLE_API_KEY", "SMARTLEAD_API_KEY"} {
		if v, err := ws.Get(ctx, "w1", k); err != nil || v != "" {
			t.Fatalf("%s resolved from global/env: %q %v", k, v, err)
		}
	}
	if err := r.Set(ctx, "w1", "AIRTABLE_API_KEY", "own-key"); err != nil {
		t.Fatal(err)
	}
	if v, _ := ws.Get(ctx, "w1", "AIRTABLE_API_KEY"); v != "own-key" {
		t.Fatalf("own key %q", v)
	}
	if v, _ := ws.Get(ctx, "w2", "AIRTABLE_API_KEY"); v != "" {
		t.Fatalf("another workspace resolved %q", v)
	}
	if v, _ := (workspaceSecrets{}).Get(ctx, "w1", "AIRTABLE_API_KEY"); v != "" {
		t.Fatal("nil resolver resolved a key")
	}

	hs := &hubspotFake{}
	port := crmPort{d: crmpush.Deps{HubSpot: hs}, sec: r}
	res := port.Push(ctx, "w1", map[string]any{"type": "hubspot"}, map[string]any{"email": "a@b.co"}, "wb", 1)
	if res.Success || hs.pushed != 0 {
		t.Fatalf("global HubSpot token used: %+v pushed=%d", res, hs.pushed)
	}
	if err := r.Set(ctx, "w1", "HUBSPOT_TOKEN", "own-token"); err != nil {
		t.Fatal(err)
	}
	port.Push(ctx, "w1", map[string]any{"type": "hubspot"}, map[string]any{"email": "a@b.co"}, "wb", 1)
	if hs.pushed != 1 {
		t.Fatalf("workspace token did not reach the client: pushed=%d", hs.pushed)
	}
}
