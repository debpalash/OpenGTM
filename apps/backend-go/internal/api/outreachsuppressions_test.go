package api

import "testing"

func TestParseSuppressionAddRequiresAddress(t *testing.T) {
	for _, in := range []string{`{"email":"nope"}`, `{"email":"@x.com"}`, `{"email":"a@"}`, `{"email":"a@b@c.com"}`} {
		if _, errs := parseSuppressionAdd([]byte(in)); errs == nil {
			t.Errorf("%s accepted", in)
		}
	}
	if got, errs := parseSuppressionAdd([]byte(`{"email":"A@Example.com"}`)); errs != nil || got != "A@Example.com" {
		t.Errorf("valid: %q %v", got, errs)
	}
}
