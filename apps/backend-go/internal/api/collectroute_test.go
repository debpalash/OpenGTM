package api

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
)

// Expected strings are json.dumps(decide_collection(q, i).to_dict()) from the Python reference
// (default separators ", " and ": ", ensure_ascii=True).
func TestCollectIntentDetailsIsPythonDumps(t *testing.T) {
	intent := "market_search"
	cases := []struct {
		query  string
		intent *string
		want   string
	}{
		{"東京のIT企業 café Zürich", nil, `{"query": "\u6771\u4eac\u306eIT\u4f01\u696d caf\u00e9 Z\u00fcrich", "intent": "market_search", "domain": "", "entity": "", "clarification_kind": "", "clarification_required": false, "reason": "", "options": []}`},
		{"decision makers at Acme Robotics", nil, `{"query": "decision makers at Acme Robotics", "intent": "people_at_company", "domain": "", "entity": "", "clarification_kind": "", "clarification_required": true, "reason": "This request needs a specialized GTM workflow and cannot safely run through the broad lead collector.", "options": [{"key": "people_at_company", "intent": "people_at_company", "label": "Find people in Chat", "description": "Find named, attributable decision-makers.", "route": "/chat", "draft": "decision makers at Acme Robotics"}]}`},
		{"IT staffing companies in Pune", &intent, `{"query": "IT staffing companies in Pune", "intent": "market_search", "domain": "", "entity": "", "clarification_kind": "", "clarification_required": false, "reason": "", "options": []}`},
	}
	for _, c := range cases {
		if got := collectIntentDetails(collectintent.Decide(c.query, c.intent)); got != c.want {
			t.Errorf("%q:\n got %s\nwant %s", c.query, got, c.want)
		}
	}
}
