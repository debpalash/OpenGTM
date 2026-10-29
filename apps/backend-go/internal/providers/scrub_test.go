package providers

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestScrubCoversEncodedForms(t *testing.T) {
	const sec = `sk/live+key&x="y"<z> é`
	jb, _ := json.Marshal(sec)
	forms := map[string]string{
		"raw":         sec,
		"query":       url.QueryEscape(sec),
		"path":        url.PathEscape(sec),
		"json":        string(jb[1 : len(jb)-1]),
		"bearer":      "Bearer " + sec,
		"bearer-enc":  "Bearer " + url.QueryEscape(sec),
		"lower-query": strings.ToLower(url.QueryEscape(sec)),
	}
	for name, form := range forms {
		got := Scrub("upstream said: "+form+" end", sec)
		if strings.Contains(got, sec) || strings.Contains(got, url.QueryEscape(sec)) ||
			strings.Contains(got, url.PathEscape(sec)) || strings.Contains(got, string(jb[1:len(jb)-1])) ||
			strings.Contains(strings.ToLower(got), strings.ToLower(url.QueryEscape(sec))) {
			t.Errorf("%s form leaked: %q", name, got)
		}
	}
	if got := Scrub("plain text", sec); got != "plain text" {
		t.Errorf("got %q", got)
	}
	if got := Scrub("x", ""); got != "x" {
		t.Errorf("empty secret: %q", got)
	}
}
