package api

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
)

// TestInsufficientDetailNeverEncodesInf: a +Inf requirement (billing treats
// Inf as insufficient) must never reach the wire as Infinity/NaN, which is not
// JSON; the amounts are clamped to the largest float64 and the key order and
// Python float spelling are kept.
func TestInsufficientDetailNeverEncodesInf(t *testing.T) {
	for _, c := range []struct {
		name         string
		bal, req     float64
		wantBody     string
		wantNoSubstr []string
	}{
		{"plain", 10, 12.5, `{"detail":{"message":"Insufficient credits: balance $10.0000 < required $12.5000. Top up to run this enrichment.","balance_usd":10.0,"required_usd":12.5}}`, nil},
		{"inf required", 1.5, math.Inf(1), "", []string{"Infinity", "NaN", "Inf\""}},
		{"nan balance", math.NaN(), 3, "", []string{"Infinity", "NaN\""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw, err := insufficientBody(&billing.InsufficientCreditsError{BalanceUSD: c.bal, RequiredUSD: c.req})
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(raw) {
				t.Fatalf("not valid JSON: %s", raw)
			}
			if c.wantBody != "" && string(raw) != c.wantBody {
				t.Fatalf("body\n got %s\nwant %s", raw, c.wantBody)
			}
			for _, s := range c.wantNoSubstr {
				if bytes.Contains(raw, []byte(s)) {
					t.Errorf("body contains %q: %s", s, raw)
				}
			}
		})
	}
}

func TestExecutorSettingsFromEnv(t *testing.T) {
	env := map[string]string{
		"GO_EXECUTOR_ENABLED":         "Yes",
		"GO_EXECUTOR_WORKSPACES":      " ws-a, ws-b ,,",
		"GO_EXECUTOR_COLUMN_TYPES":    "formula, http ,bogus,research",
		"PROVENANCE_TRACKING_ENABLED": "off",
		"WORKBOOK_RUN_CONCURRENCY":    "7",
	}
	s := executorSettings(func(k string) string { return env[k] })
	if !s.Enabled || s.Provenance {
		t.Fatalf("flags: %+v", s)
	}
	if len(s.Cohort) != 2 || !s.Cohort["ws-a"] || !s.Cohort["ws-b"] {
		t.Fatalf("cohort: %v", s.Cohort)
	}
	// unknown types are dropped (runplan only honours formula/http/ai_formula/research/agent)
	if len(s.ExtraTypes) != 3 || !s.ExtraTypes["formula"] || !s.ExtraTypes["http"] || !s.ExtraTypes["research"] {
		t.Fatalf("extra types: %v", s.ExtraTypes)
	}
	if s.Concurrency != 7 {
		t.Fatalf("concurrency %d", s.Concurrency)
	}
	d := executorSettings(func(string) string { return "" })
	if d.Enabled || len(d.Cohort) != 0 || len(d.ExtraTypes) != 0 || d.Concurrency != 12 {
		t.Fatalf("defaults: %+v", d)
	}
	// a non-numeric or non-positive concurrency falls back to 12 (Python would crash at import; Go stays safe)
	if executorSettings(func(k string) string {
		if k == "WORKBOOK_RUN_CONCURRENCY" {
			return "abc"
		}
		return ""
	}).Concurrency != 12 {
		t.Fatal("bad concurrency must default")
	}
}

func TestExecutorSettingsNativeProviders(t *testing.T) {
	if executorSettings(func(string) string { return "" }).NativeProv {
		t.Fatal("GO_RUN_NATIVE_PROVIDERS must default off")
	}
	if !executorSettings(func(k string) string {
		if k == "GO_RUN_NATIVE_PROVIDERS" {
			return "true"
		}
		return ""
	}).NativeProv {
		t.Fatal("GO_RUN_NATIVE_PROVIDERS=true must enable")
	}
}
