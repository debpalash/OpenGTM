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
		if (err != nil) != v.Blocked {
			t.Errorf("CheckURL(%q, allowHTTP=%v) err=%v, want blocked=%v", v.URL, v.AllowHTTP, err, v.Blocked)
		}
	}
}
