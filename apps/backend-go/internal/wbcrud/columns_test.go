package wbcrud

import (
	"encoding/json"
	"testing"
)

func normalizeVector(t *testing.T, name string) (Column, []map[string]any, string) {
	t.Helper()
	for _, c := range loadVectors(t).Normalize {
		if c.Name == name {
			col, errs := NormalizeRequestColumn(json.RawMessage(c.Input))
			if len(c.Errors) > 0 {
				if !sameJSON(t, errs, c.Errors) {
					t.Errorf("%s errors = %s, want %v", name, compact(t, errs), c.Errors)
				}
				return nil, c.Errors, c.Column
			}
			if len(errs) > 0 {
				t.Fatalf("%s: unexpected errors %s", name, compact(t, errs))
			}
			return col, nil, c.Column
		}
	}
	t.Fatalf("no vector %q", name)
	return nil, nil, ""
}

func TestNormalizeAllVectors(t *testing.T) {
	for _, c := range loadVectors(t).Normalize {
		t.Run(c.Name, func(t *testing.T) {
			col, errs := NormalizeRequestColumn(json.RawMessage(c.Input))
			if len(c.Errors) > 0 {
				if !sameJSON(t, errs, c.Errors) {
					t.Errorf("errors = %s, want %v", compact(t, errs), c.Errors)
				}
				return
			}
			if len(errs) > 0 {
				t.Fatalf("unexpected errors %s", compact(t, errs))
			}
			if got := compact(t, col); got != c.Column {
				t.Errorf("column = %s\nwant      %s", got, c.Column)
			}
		})
	}
}

func TestNormalizeDefaultsAndDropsUnknown(t *testing.T) {
	col, _, want := normalizeVector(t, "defaults_and_unknown_dropped")
	if col["type"] != "lead_field" || compact(t, col) != want {
		t.Errorf("got %s want %s", compact(t, col), want)
	}
	if _, ok := col["zzz"]; ok {
		t.Error("unknown key kept")
	}
}

func TestNormalizeKeyOrderMatchesModel(t *testing.T) {
	col, _, want := normalizeVector(t, "key_order_follows_model")
	if got := compact(t, col); got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestNormalizeStrictIntRejectsString(t *testing.T) {
	_, errs := NormalizeRequestColumn(json.RawMessage(`{"id":"a","name":"A","policy":{"max_steps":"3"}}`))
	if len(errs) != 1 || errs[0].Msg != "Input should be a valid integer" || errs[0].Type != "int_type" {
		t.Fatalf("errs = %s", compact(t, errs))
	}
	normalizeVector(t, "policy_string_steps")
}

func TestNormalizeNegativeBudget422(t *testing.T) {
	normalizeVector(t, "negative_budget")
	normalizeVector(t, "negative_budget_float")
}

func TestUnknownColumnKeysPreservedOnRead(t *testing.T) {
	cols, err := ParseColumns([]byte(`[{"id":"a","name":"A","zzz":{"x":1.50,"big":12345678901234567890},"width":200}]`))
	if err != nil {
		t.Fatal(err)
	}
	got := compact(t, cols)
	want := `[{"id":"a","name":"A","width":200,"zzz":{"x":1.50,"big":12345678901234567890}}]`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if _, err := ParseColumns([]byte(`{"a":1}`)); err == nil {
		t.Error("non-array should error")
	}
	if cols, err := ParseColumns(nil); err != nil || len(cols) != 0 {
		t.Errorf("empty input: %v %v", cols, err)
	}
}
