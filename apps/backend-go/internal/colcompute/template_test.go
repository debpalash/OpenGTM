package colcompute

import (
	"errors"
	"testing"
)

type templateVectors struct {
	RowValues     map[string]any `json:"row_values"`
	ResolvePrompt []struct {
		Template string `json:"template"`
		Default  struct {
			Value   *string `json:"value"`
			Message string  `json:"message"`
		} `json:"default"`
		Strict struct {
			Value   *string `json:"value"`
			Message string  `json:"message"`
		} `json:"strict"`
	} `json:"resolve_prompt"`
	CellText []struct {
		Value any    `json:"value"`
		Text  string `json:"text"`
	} `json:"cell_text"`
	GetRowValues []struct {
		Name          string           `json:"name"`
		Cells         map[string]any   `json:"cells"`
		ColumnsConfig []map[string]any `json:"columns_config"`
		Values        struct {
			Value map[string]string `json:"value"`
		} `json:"values"`
	} `json:"get_row_values"`
}

func TestResolvePromptVectors(t *testing.T) {
	var v templateVectors
	loadVectors(t, "template_vectors.json", &v)
	for _, c := range v.ResolvePrompt {
		for _, strict := range []bool{false, true} {
			want := c.Default
			if strict {
				want = c.Strict
			}
			got, err := ResolvePrompt(c.Template, v.RowValues, strict)
			if want.Value == nil {
				var ve *ValueError
				if !errors.As(err, &ve) || ve.Error() != want.Message {
					t.Errorf("ResolvePrompt(%q, strict=%v) = %q, %v; want ValueError %q", c.Template, strict, got, err, want.Message)
				}
				continue
			}
			if err != nil || got != *want.Value {
				t.Errorf("ResolvePrompt(%q, strict=%v) = %q, %v; want %q", c.Template, strict, got, err, *want.Value)
			}
		}
	}
}

func TestCellTextVectors(t *testing.T) {
	var v templateVectors
	loadVectors(t, "template_vectors.json", &v)
	for _, c := range v.CellText {
		if got := CellText(c.Value); got != c.Text {
			t.Errorf("CellText(%v) = %q, want %q", c.Value, got, c.Text)
		}
	}
}

func TestRowValuesVectors(t *testing.T) {
	var v templateVectors
	loadVectors(t, "template_vectors.json", &v)
	for _, c := range v.GetRowValues {
		t.Run(c.Name, func(t *testing.T) {
			cols := make([]ColumnInfo, 0, len(c.ColumnsConfig))
			for _, cc := range c.ColumnsConfig {
				id, _ := cc["id"].(string)
				name, _ := cc["name"].(string)
				cols = append(cols, ColumnInfo{ID: id, Name: name})
			}
			got := RowValues(c.Cells, cols)
			if len(got) != len(c.Values.Value) {
				t.Fatalf("got %v, want %v", got, c.Values.Value)
			}
			for k, w := range c.Values.Value {
				if g, ok := got[k]; !ok || g != w {
					t.Fatalf("key %q: got %q (present %v), want %q", k, g, ok, w)
				}
			}
		})
	}
}
