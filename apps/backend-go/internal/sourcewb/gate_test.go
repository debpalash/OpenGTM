package sourcewb

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// TestProductionGateMatchesRecordedDecisions: the wired validator (internal/collect) and account
// fit (internal/leadpeople) reproduce the decisions Python made for the scenario leads.
func TestProductionGateMatchesRecordedDecisions(t *testing.T) {
	_, raws := loadVectors(t)
	checked := 0
	for _, raw := range raws {
		var sc struct {
			Name string                       `json:"name"`
			Seed map[string][]json.RawMessage `json:"seed"`
			Gate map[string][2]any            `json:"gate"`
			Fit  map[string]json.RawMessage   `json:"fit"`
		}
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatal(err)
		}
		var brief *wbcrud.Obj
		for _, w := range sc.Seed["workbooks"] {
			var wb struct {
				Columns []json.RawMessage `json:"columns_config"`
			}
			_ = json.Unmarshal(w, &wb)
			for _, c := range wb.Columns {
				o, _ := wbcrud.ParseObject(c)
				if b, ok := asObj(get(o, "account_discovery_brief")); ok {
					brief = b
				}
			}
		}
		for _, lr := range sc.Seed["leads"] {
			lead, err := wbcrud.ParseObject(lr)
			if err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprint(get(lead, "id"))
			if want, ok := sc.Gate[id]; ok {
				gotOK, gotReason := CollectValidateLight(lead)
				if gotOK != want[0] || gotReason != want[1] {
					t.Errorf("%s lead %s: validate_lead_light = %v %q want %v", sc.Name, id, gotOK, gotReason, want)
				}
				checked++
			}
			if want, ok := sc.Fit[id]; ok && brief != nil {
				got, err := AccountFitFunc(lead, brief)
				if err != nil {
					t.Fatal(err)
				}
				var g, w map[string]any
				gb, _ := got.MarshalJSON()
				_ = json.Unmarshal(gb, &g)
				_ = json.Unmarshal(want, &w)
				delete(g, "evaluated_at")
				delete(w, "evaluated_at")
				gs, _ := json.Marshal(g)
				ws, _ := json.Marshal(w)
				if string(gs) != string(ws) {
					t.Errorf("%s lead %s: fit\n got %s\nwant %s", sc.Name, id, gs, ws)
				}
				checked++
			}
		}
	}
	if checked < 10 {
		t.Fatalf("only %d decisions checked", checked)
	}
}
