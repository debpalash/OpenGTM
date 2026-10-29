package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestFieldErrorCtxKeysInPydanticOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	ValidationError(rec, FieldError{Type: "too_long", Loc: []any{"body", "steps"}, Msg: "m <x>", Input: "i",
		Ctx: map[string]any{"actual_length": 9, "max_length": 8, "field_type": "List"}},
		FieldError{Type: "x", Msg: "y"})
	want := `{"detail":[{"type":"too_long","loc":["body","steps"],"msg":"m <x>","input":"i","ctx":{"field_type":"List","max_length":8,"actual_length":9}},{"type":"x","loc":[],"msg":"y","input":null}]}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
