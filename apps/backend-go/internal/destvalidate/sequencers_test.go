package destvalidate

import (
	"strings"
	"testing"
)

func TestHeyReachLemlistConfig(t *testing.T) {
	cases := []struct {
		typ  string
		cfg  map[string]any
		want string // error substring; "" is valid
	}{
		{"heyreach", map[string]any{"campaign_id": "235", "linkedin_account_id": "123"}, ""},
		{"heyreach", map[string]any{"campaign_id": "235"}, "heyreach linkedin_account_id is required"},
		{"heyreach", map[string]any{"linkedin_account_id": "1"}, "heyreach campaign_id is required"},
		{"heyreach", map[string]any{"campaign_id": "abc", "linkedin_account_id": "1"}, "heyreach campaign_id must be a positive integer"},
		{"heyreach", map[string]any{"campaign_id": "1", "linkedin_account_id": "-2"}, "heyreach linkedin_account_id must be a positive integer"},
		{"heyreach", map[string]any{"campaign_id": "1", "linkedin_account_id": "2", "settings": map[string]any{}}, "unsupported heyreach config: settings"},
		{"heyreach", map[string]any{"campaign_id": "1", "linkedin_account_id": "2", "api_key": "x"}, "credentials must use workspace secrets"},
		{"heyreach", map[string]any{"campaign_id": float64(235), "linkedin_account_id": float64(7)}, ""},
		{"heyreach", map[string]any{"campaign_id": 1.5, "linkedin_account_id": "7"}, "heyreach campaign_id must be a positive integer"},
		{"lemlist", map[string]any{"campaign_id": "cam_1"}, ""},
		{"lemlist", map[string]any{}, "lemlist campaign_id is required"},
		{"lemlist", map[string]any{"campaign_id": "cam_1", "skip_if_in_campaign": true}, "unsupported lemlist config: skip_if_in_campaign"},
	}
	for _, c := range cases {
		_, err := ValidateConfig(c.typ, c.cfg)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s %v: %v", c.typ, c.cfg, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s %v: got %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
	if !IsType("heyreach") || !IsType("lemlist") {
		t.Error("types not registered")
	}
}
