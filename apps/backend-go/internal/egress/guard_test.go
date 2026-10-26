package egress

import (
	"encoding/json"
	"os"
	"testing"
)

type vector struct {
	URL       string `json:"url"`
	AllowHTTP bool   `json:"allow_http"`
	Blocked   bool   `json:"blocked"`
	// GoOnlyBlocked marks a Python-pinned row that Go deliberately blocks (documented deviation).
	GoOnlyBlocked bool `json:"go_only_blocked"`
}

func TestCheckURLVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/url_guard_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 40 {
		t.Fatalf("need >=40 vectors, have %d", len(vs))
	}
	for _, v := range vs {
		err := CheckURL(v.URL, v.AllowHTTP)
		want := v.Blocked || v.GoOnlyBlocked
		if (err != nil) != want {
			t.Errorf("CheckURL(%q, allowHTTP=%v) err=%v, want blocked=%v", v.URL, v.AllowHTTP, err, want)
		}
	}
}

// Go-only deviations from Python's is_private (documented in the runbook): CGNAT
// 100.64.0.0/10 (Alibaba metadata 100.100.100.200, Tailscale, carrier NAT) and
// NAT64 64:ff9b::/96 are refused. Python-pinned vectors stay in url_guard_vectors.json.
func TestCheckURLGoOnlyVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/url_guard_go_only_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if err := CheckURL(v.URL, v.AllowHTTP); (err != nil) != v.Blocked {
			t.Errorf("CheckURL(%q) err=%v, want blocked=%v", v.URL, err, v.Blocked)
		}
	}
}
