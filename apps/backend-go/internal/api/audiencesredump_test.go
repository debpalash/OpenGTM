package api

import (
	"encoding/json"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
)

// Python decodes the stored json columns and FastAPI re-dumps them (ensure_ascii off, float repr):
// rows written by Python hold \u escapes, and a client's 1E2 comes back as 100.0.
func TestAudienceDestinationJSONColumnsRedumpLikePython(t *testing.T) {
	a := audJSON(gen.Audience{ID: "a", Filters: []byte(`{"city":"München","min":1E2}`)})
	if got := string(a.Filters); got != `{"city":"München","min":100.0}` {
		t.Errorf("audience filters %s", got)
	}
	d := destView(deststore.Destination{ID: "d",
		Config:   []byte(`{"token":"x","name":"München","n":1E2}`),
		FieldMap: []byte(`{"co":"é","n":1E2}`)})
	if got := string(d.Config); got != `{"name":"München","n":100.0}` {
		t.Errorf("config %s", got)
	}
	if got := string(d.FieldMap); got != `{"co":"é","n":100.0}` {
		t.Errorf("field_map %s", got)
	}
	if _, err := json.Marshal(d); err != nil {
		t.Fatal(err)
	}
}
