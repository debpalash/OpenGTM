package outurl_test

import (
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook/outurl"
)

// subst is a stand-in for rowtmpl.Resolve: {name} -> row[name].
func subst(tmpl string, row map[string]any) (string, error) {
	for k, v := range row {
		s, _ := v.(string)
		tmpl = strings.ReplaceAll(tmpl, "{"+k+"}", s)
	}
	return tmpl, nil
}

func TestValidateTemplate(t *testing.T) {
	for _, ok := range []string{
		"https://hooks.example.com/a/{id}", "https://hooks.example.com?x={a}", "https://hooks.example.com#{a}",
		"http://h.example.com:8080/{a}", "https://h.example.com/no-placeholder", "",
	} {
		if err := outurl.ValidateTemplate(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"https://{sub}.example.com/hook", "{website}/hook", "{scheme}://h.example.com/x", "https://h.example.com:{port}/x",
		"https://{user}@h.example.com/x", "https://h.example.com{path}", "https://user:{pw}@h.example.com/x", "//{h}/x",
	} {
		if err := outurl.ValidateTemplate(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestResolveEncodesAndKeepsHost(t *testing.T) {
	row := map[string]any{"id": "a b/c?d#e", "n": 5, "m": map[string]any{"k": "v/w"}}
	got, err := outurl.Resolve("https://h.example.com/x/{id}?q={id}&n={n}", row, subst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "https://h.example.com/x/a%20b%2Fc%3Fd%23e?q=a%20b%2Fc%3Fd%23e") {
		t.Fatalf("got %q", got)
	}
	if _, err := outurl.Resolve("https://{id}.example.com/x", row, subst); err == nil {
		t.Fatal("placeholder in host accepted")
	}
	// a value that would end the host is neutralised
	got, err = outurl.Resolve("https://h.example.com/{id}", map[string]any{"id": "evil.com/@x"}, subst)
	if err != nil || strings.Contains(got, "evil.com/@") {
		t.Fatalf("got %q err %v", got, err)
	}
	// no placeholder: unchanged
	if got, err := outurl.Resolve("https://h.example.com/x", row, subst); err != nil || got != "https://h.example.com/x" {
		t.Fatalf("%q %v", got, err)
	}
}
