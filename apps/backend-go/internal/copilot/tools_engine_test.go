package copilot

import (
	"context"
	"testing"
)

// Arguments Go does not answer exactly like Python fall through to the worker (handled=false), before any
// database or upstream call.
func TestEngineToolsFallThroughOnUnmirroredArgs(t *testing.T) {
	tn := &Turn{Deps: Deps{Engine: &Engine{}}, WS: "ws", Slug: "s"}
	cases := []struct {
		name string
		args map[string]any
	}{
		{"set_workbook_refresh", map[string]any{"workbook_id": 5}},
		{"add_agent_column", map[string]any{"workbook_id": "w", "max_cost_usd": []any{}}},
		{"add_signal_trigger", map[string]any{"workbook_id": 1}},
		{"start_collection", map[string]any{"query": "x"}},                                   // SQLite collection ledger
		{"create_source_workbook", map[string]any{"icp_description": "x", "name": []any{1}}}, // non-string name
		{"create_source_workbook", map[string]any{"icp_description": "x", "target_rows": "ten"}},
		{"ambitionbox_search", map[string]any{}}, // no client configured
		{"import_ambitionbox_to_workbook", map[string]any{"limit": []any{}}},
		{"enrich_lead", map[string]any{"lead_id": int64(1)}}, // no Providers configured
	}
	for _, c := range cases {
		res, handled, err := tn.engineTool(context.Background(), parsedCall{Name: c.name, Args: c.args})
		if handled || err != nil {
			t.Errorf("%s %v: handled=%v err=%v res=%s", c.name, c.args, handled, err, res)
		}
	}
	if !isEngineTool("enrich_lead") || !isEngineTool("track_account_signals") || !isEngineTool("create_source_workbook") {
		t.Error("engine tool set")
	}
	// KeyError texts are answered in Go like execute()'s generic failure.
	res, handled, _ := tn.engineTool(context.Background(), parsedCall{Name: "ambitionbox_jobs", Args: map[string]any{}})
	if handled {
		t.Errorf("no client: %s", res)
	}
	tn.Engine.Ambition = &fakeABUnit{}
	res, handled, _ = tn.engineTool(context.Background(), parsedCall{Name: "ambitionbox_jobs", Args: map[string]any{}})
	if !handled || res != `{"error": "Tool 'ambitionbox_jobs' failed: 'company_id'"}` {
		t.Errorf("missing company_id: %v %s", handled, res)
	}
}

type fakeABUnit struct{ AmbitionClient }
