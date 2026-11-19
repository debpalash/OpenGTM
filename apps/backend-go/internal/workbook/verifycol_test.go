package workbook

import "testing"

func TestVerifyColumnIsOrderedAfterTheEmailColumn(t *testing.T) {
	cols, err := parseColumns([]byte(`[
	  {"id":"ver","type":"waterfall","name":"Email Verify","target_field":"email_verify"},
	  {"id":"em","type":"waterfall","name":"Email","target_field":"email"}]`))
	if err != nil {
		t.Fatal(err)
	}
	ordered := orderColumns(cols)
	if ordered[0].ID != "em" || ordered[1].ID != "ver" {
		t.Fatalf("order %s, %s: the verify column must run after the Email column", ordered[0].ID, ordered[1].ID)
	}
	if _, ok := cols[0].cfg["input_columns"]; ok {
		t.Fatal("inference must not rewrite the stored column configuration")
	}
}

func TestVerifyColumnWithTwoEmailColumnsIsNotGuessed(t *testing.T) {
	cols, _ := parseColumns([]byte(`[
	  {"id":"a","type":"waterfall","target_field":"email"},{"id":"b","type":"waterfall","target_field":"email"},
	  {"id":"ver","type":"waterfall","target_field":"email_verify"}]`))
	if _, linked := cols[2].g.Lookup("input_columns"); linked {
		t.Fatal("an ambiguous source must stay unlinked")
	}
}
