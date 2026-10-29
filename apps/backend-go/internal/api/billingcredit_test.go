package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeObj(t *testing.T, s string) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	m := map[string]any{}
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCheckoutCreditCappedByAmountTotal(t *testing.T) {
	for name, tc := range map[string]struct {
		obj  string
		want float64
		ok   bool
	}{
		"metadata above paid is capped": {`{"id":"s","amount_total":12,"currency":"usd","metadata":{"workspace_id":"w","credit_usd":"0.125"}}`, 0.12, true},
		"metadata below paid kept":      {`{"id":"s","amount_total":5000,"currency":"usd","metadata":{"workspace_id":"w","credit_usd":"25"}}`, 25, true},
		"uppercase currency":            {`{"id":"s","amount_total":1000,"currency":"USD","metadata":{"workspace_id":"w","credit_usd":"99"}}`, 10, true},
		"no amount_total as today":      {`{"id":"s","metadata":{"workspace_id":"w","credit_usd":"99"}}`, 99, true},
		"non-usd as today":              {`{"id":"s","amount_total":100,"currency":"eur","metadata":{"workspace_id":"w","credit_usd":"99"}}`, 99, true},
		"zero paid credits nothing":     {`{"id":"s","amount_total":0,"currency":"usd","metadata":{"workspace_id":"w","credit_usd":"5"}}`, 0, false},
	} {
		_, _, got, ok := checkoutFromEvent(decodeObj(t, tc.obj))
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: got %v,%v want %v,%v", name, got, ok, tc.want, tc.ok)
		}
	}
}
