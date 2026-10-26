package wbcrud

import "testing"

func withLimits(t *testing.T, l Limits) {
	t.Helper()
	SetLimits(l)
	t.Cleanup(func() { SetLimits(Limits{}) })
}

func TestAddRowsRowCountCap(t *testing.T) {
	withLimits(t, Limits{MaxAddRows: 3})
	doc, _ := DecodeBody([]byte(`{"rows":[{},{},{},{}]}`))
	if _, errs := ParseAddRows(doc); len(errs) != 1 || errs[0].Type != "too_long" {
		t.Fatalf("over cap: %+v", errs)
	}
	doc, _ = DecodeBody([]byte(`{"rows":[{},{},{}]}`))
	if _, errs := ParseAddRows(doc); errs != nil {
		t.Fatalf("at cap: %+v", errs)
	}
}

func TestAddRowsKeyCap(t *testing.T) {
	withLimits(t, Limits{MaxRowKeys: 3})
	doc, _ := DecodeBody([]byte(`{"rows":[{"a":1,"b":2,"c":3,"d":4}]}`))
	if _, errs := ParseAddRows(doc); len(errs) != 1 || errs[0].Type != "too_long" {
		t.Fatalf("top-level keys: %+v", errs)
	}
	doc, _ = DecodeBody([]byte(`{"rows":[{"data":{"a":1,"b":2,"c":3,"d":4}}]}`))
	if _, errs := ParseAddRows(doc); len(errs) != 1 || errs[0].Type != "too_long" {
		t.Fatalf("data keys: %+v", errs)
	}
	doc, _ = DecodeBody([]byte(`{"rows":[{"a":1,"b":2,"c":3}]}`))
	if _, errs := ParseAddRows(doc); errs != nil {
		t.Fatalf("at cap: %+v", errs)
	}
}
