package crmpush

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
)

type fakeAttio struct {
	connected bool
	res       crm.AttioPushResult
	leads     []crm.Lead
	maps      []crm.FieldMap
}

func (f *fakeAttio) IsConnected(context.Context, string) bool { return f.connected }
func (f *fakeAttio) PushLeadAsPerson(_ context.Context, _ string, l crm.Lead, fm crm.FieldMap) crm.AttioPushResult {
	f.leads, f.maps = append(f.leads, l), append(f.maps, fm)
	return f.res
}

func TestPushAttio(t *testing.T) {
	ctx := context.Background()
	f := &fakeAttio{connected: true, res: crm.AttioPushResult{Success: true, Action: "upserted", AttioID: "rec-1"}}
	cfg := map[string]any{"type": "attio", "field_map": map[string]any{"email": "email_addresses"}}
	// A row that names an Attio external id still upserts: there is no update-by-id path for Attio.
	lead := map[string]any{"email": "a@b.co", "crm_external_id": "x", "crm_type": "attio"}
	r := Push(ctx, Deps{Attio: f}, "ws", cfg, lead, "", NoLeadID)
	if !r.Success || r.Value != "Attio: upserted rec-1" || len(f.leads) != 1 || len(f.maps[0]) != 1 {
		t.Fatalf("%+v", r)
	}
	if _, ok := f.leads[0].Get("crm_external_id"); ok {
		t.Error("crm_external_id leaked into the lead")
	}

	f.res = crm.AttioPushResult{Error: "HTTP 409: multiple_match_results"}
	if r = Push(ctx, Deps{Attio: f}, "ws", cfg, lead, "", NoLeadID); r.Success || r.Error != "HTTP 409: multiple_match_results" {
		t.Fatalf("%+v", r)
	}
	f.res = crm.AttioPushResult{}
	if r = Push(ctx, Deps{Attio: f}, "ws", cfg, lead, "", NoLeadID); r.Error != "Attio push failed" {
		t.Fatalf("%+v", r)
	}
	f.connected = false
	if r = Push(ctx, Deps{Attio: f}, "ws", cfg, lead, "", NoLeadID); r.Error != "Attio not connected" {
		t.Fatalf("%+v", r)
	}
	if r = Push(ctx, Deps{}, "ws", cfg, lead, "", NoLeadID); r.Error != "Attio not connected" {
		t.Fatalf("%+v", r)
	}
}
