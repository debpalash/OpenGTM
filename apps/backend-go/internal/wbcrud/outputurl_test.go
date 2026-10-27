package wbcrud

import (
	"strings"
	"testing"
)

func outCol(t *testing.T, id, dest, cfg string) Column {
	t.Helper()
	c, errs := NormalizeRequestColumn([]byte(`{"id":"`+id+`","name":"`+id+`","type":"output","destination":"`+dest+`","destination_config":`+cfg+`}`))
	if len(errs) > 0 {
		t.Fatalf("%+v", errs)
	}
	return c
}

func TestValidateOutputColumnsRefusesNonLiteralWebhookURL(t *testing.T) {
	for _, url := range []string{
		"https://{sub}.example.com/hook", "{website}/hook", "{scheme}://h.example.com/x",
		"https://h.example.com:{port}/x", "https://{user}@h.example.com/x",
	} {
		g := ValidateOutputColumns(nil, []Column{outCol(t, "o1", "webhook", `{"url":"`+url+`"}`)})
		if g == nil || g.Status != 422 || !strings.Contains(g.Detail, "o1") || strings.Contains(g.Detail, url) {
			t.Errorf("%q: %+v", url, g)
		}
	}
}

func TestValidateOutputColumnsAccepts(t *testing.T) {
	ok := []Column{
		outCol(t, "a", "webhook", `{"url":"https://h.example.com/a/{id}?q={id}"}`),
		outCol(t, "b", "webhook", `{"url":"https://h.example.com/plain"}`),
		outCol(t, "c", "webhook", `{}`),
		outCol(t, "d", "slack", `{"url":"https://{x}.example.com"}`), // not a webhook: no url template
	}
	if g := ValidateOutputColumns(nil, ok); g != nil {
		t.Fatalf("%+v", g)
	}
}

// A stored column that is already invalid does not block unrelated saves; only a changed output column is checked.
func TestValidateOutputColumnsChecksOnlyChanged(t *testing.T) {
	bad := outCol(t, "o", "webhook", `{"url":"https://{h}.example.com/x"}`)
	if g := ValidateOutputColumns([]Column{bad}, []Column{bad.Clone()}); g != nil {
		t.Fatalf("unchanged column refused: %+v", g)
	}
	fixed := outCol(t, "o", "webhook", `{"url":"https://h.example.com/{x}"}`)
	if g := ValidateOutputColumns([]Column{bad}, []Column{fixed}); g != nil {
		t.Fatalf("%+v", g)
	}
	if g := ValidateOutputColumns([]Column{fixed}, []Column{bad}); g == nil {
		t.Fatal("changed to invalid accepted")
	}
}
