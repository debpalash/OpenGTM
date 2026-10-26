package runplan

import "testing"

func TestCellScopeErrors(t *testing.T) {
	ok := map[string][]string{"1": {"a"}, "22": {}, "9999999999999999999999": {"b"}}
	if err := ValidateCellScope(ok); err != nil {
		t.Fatalf("valid scope rejected: %v", err)
	}
	bad := []struct {
		name string
		in   map[string][]string
		want string
	}{
		{"zero", map[string][]string{"0": {"a"}}, "Invalid row identity in cell scope"},
		{"leading zero", map[string][]string{"01": {"a"}}, "Invalid row identity in cell scope"},
		{"negative", map[string][]string{"-1": {"a"}}, "Invalid row identity in cell scope"},
		{"text", map[string][]string{"x": {"a"}}, "Invalid row identity in cell scope"},
		{"empty key", map[string][]string{"": {"a"}}, "Invalid row identity in cell scope"},
		{"plus", map[string][]string{"+1": {"a"}}, "Invalid row identity in cell scope"},
		{"arabic digit", map[string][]string{"\u0661": {"a"}}, "Invalid row identity in cell scope"},
		{"empty column id", map[string][]string{"1": {""}}, "Invalid column identities in cell scope"},
	}
	for _, c := range bad {
		err := ValidateCellScope(c.in)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: got %v want %q", c.name, err, c.want)
		}
	}
}
