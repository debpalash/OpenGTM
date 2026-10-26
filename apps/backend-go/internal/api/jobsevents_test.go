package api

import "testing"

// Python returns json.loads of the stored text, so the keys keep stored order; the response is
// starlette's compact ensure_ascii=False dump with floats in repr form.
func TestCompactStoredEventKeepsKeyOrder(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"type": "stage_start", "ts": 1788000004.0, "job_id": "j1", "message": "café <b> & \"q\" \\ \n", "workspace_id": "w", "nested": {"z": [1, 2.5, null, true], "a": {}}}`,
			`{"type":"stage_start","ts":1788000004.0,"job_id":"j1","message":"café <b> & \"q\" \\ \n","workspace_id":"w","nested":{"z":[1,2.5,null,true],"a":{}}}`},
		{`{"b":1,"a":2}`, `{"b":1,"a":2}`},
		{`{"m": "  "}`, "{\"m\":\"  \"}"},
	} {
		got, ok := compactStoredEvent(c.in)
		if !ok || string(got) != c.want {
			t.Errorf("compactStoredEvent(%q) = %q %v, want %q", c.in, got, ok, c.want)
		}
	}
	if _, ok := compactStoredEvent(`{"a": `); ok {
		t.Error("truncated JSON must not compact")
	}
}
