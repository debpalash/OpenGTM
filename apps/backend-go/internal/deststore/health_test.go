package deststore_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
)

func TestHealthSuccessRateRounding(t *testing.T) {
	var fx struct {
		SuccessRate []struct {
			Succeeded   int      `json:"succeeded"`
			Total       int      `json:"total"`
			SuccessRate *float64 `json:"success_rate"`
		} `json:"success_rate"`
		Consecutive []struct {
			LatestFirst []string `json:"latest_first"`
			Want        int      `json:"consecutive_unhealthy_runs"`
		} `json:"consecutive"`
	}
	b, err := os.ReadFile("testdata/health_shapes.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.SuccessRate) < 10 || len(fx.Consecutive) < 5 {
		t.Fatalf("fixture too small: %d/%d", len(fx.SuccessRate), len(fx.Consecutive))
	}
	for _, c := range fx.SuccessRate {
		got := deststore.SuccessRate(c.Succeeded, c.Total)
		if (got == nil) != (c.SuccessRate == nil) || (got != nil && *got != *c.SuccessRate) {
			t.Errorf("SuccessRate(%d,%d) = %v, want %v", c.Succeeded, c.Total, got, c.SuccessRate)
		}
	}
	for _, c := range fx.Consecutive {
		if got := deststore.ConsecutiveUnhealthy(c.LatestFirst); got != c.Want {
			t.Errorf("ConsecutiveUnhealthy(%v) = %d, want %d", c.LatestFirst, got, c.Want)
		}
	}
	// the window is the newest 20 only
	many := make([]string, 30)
	for i := range many {
		many[i] = "failed"
	}
	if got := deststore.ConsecutiveUnhealthy(many); got != 20 {
		t.Errorf("window = %d, want 20", got)
	}
}

func TestSafeConfigRedactsTopLevelSecrets(t *testing.T) {
	d := deststore.Destination{Config: json.RawMessage(`{"b": 1, "token": "SECRET-T", "url": "https://x.test", "api_key": "SECRET-K", "secret": "SECRET-S", "a": {"token": "kept-nested"}}`)}
	got := string(d.SafeConfig())
	for _, leak := range []string{"SECRET-T", "SECRET-K", "SECRET-S"} {
		if strings.Contains(got, leak) {
			t.Fatalf("SafeConfig leaked a secret value: %s", got)
		}
	}
	var v, want any
	_ = json.Unmarshal([]byte(got), &v)
	_ = json.Unmarshal([]byte(`{"b": 1, "url": "https://x.test", "a": {"token": "kept-nested"}}`), &want)
	if !reflect.DeepEqual(v, want) {
		t.Errorf("SafeConfig = %s", got)
	}
	if !(strings.Index(got, `"b"`) < strings.Index(got, `"url"`) && strings.Index(got, `"url"`) < strings.Index(got, `"a"`)) {
		t.Errorf("stored key order lost: %s", got)
	}
	for _, in := range []string{``, `null`, `[]`} {
		if out := string(deststore.Destination{Config: json.RawMessage(in)}.SafeConfig()); out != `{}` {
			t.Errorf("SafeConfig(%q) = %s, want {}", in, out)
		}
	}
}
