package api

import (
	"encoding/json"
	"testing"
)

func TestEventTruthyMatchesPython(t *testing.T) {
	falsy := []any{nil, false, "", json.Number("0"), json.Number("0.0"), json.Number("-0"), json.Number("0e5"), []any{}, map[string]any{}, 0, int64(0), 0.0, []string{}}
	truthy := []any{true, "x", " ", json.Number("7"), json.Number("-1.5"), json.Number("1e-9"), []any{nil}, map[string]any{"a": 1}, 3, 0.5, []string{"a"}}
	for _, v := range falsy {
		if eventTruthy(v) {
			t.Errorf("%#v should be falsy", v)
		}
	}
	for _, v := range truthy {
		if !eventTruthy(v) {
			t.Errorf("%#v should be truthy", v)
		}
	}
}
