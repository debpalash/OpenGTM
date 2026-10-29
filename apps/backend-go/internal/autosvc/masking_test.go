package autosvc

import (
	"strings"
	"testing"
)

// S-L3: viewers see webhook headers and URL path/query masked; managers and the
// stored rule are untouched.
func TestTriggerMaskedForViewers(t *testing.T) {
	actions, err := ParseJSON([]byte(`[
	 {"type":"webhook","config":{"url":"https://hooks.slack.com/services/T0/B0/SECRETPATH?k=v","headers":{"Authorization":"Bearer abc","X-A":"b"},"header_secret_ref":"WEBHOOK_TOKEN","method":"POST"}},
	 {"type":"re_enrich","config":{"column_ids":["c1"]}}]`))
	if err != nil {
		t.Fatal(err)
	}
	tr := Trigger{ID: "t1", Actions: actions, TriggerConfig: emptyObj()}
	before := string(marshal(tr))
	m := string(marshal(tr.Masked()))
	for _, leak := range []string{"SECRETPATH", "Bearer abc", "k=v", `"b"`} {
		if strings.Contains(m, leak) {
			t.Errorf("masked output leaks %q: %s", leak, m)
		}
	}
	for _, keep := range []string{"https://hooks.slack.com/***", `"Authorization":"***"`, `"X-A":"***"`, "WEBHOOK_TOKEN", `"column_ids":["c1"]`, `"method":"POST"`} {
		if !strings.Contains(m, keep) {
			t.Errorf("masked output lost %q: %s", keep, m)
		}
	}
	if after := string(marshal(tr)); after != before {
		t.Errorf("Masked mutated the original:\n%s\n%s", before, after)
	}
}
