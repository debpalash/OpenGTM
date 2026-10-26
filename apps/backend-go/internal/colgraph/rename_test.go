package colgraph

import (
	"reflect"
	"testing"
)

func TestBrokenRenamesIndexesLostAliasReferences(t *testing.T) {
	before := []Col{
		MakeCol("id", "a", "name", "Company Name", "type", "lead_field"),
		MakeCol("id", "b", "type", "formula", "formula", "{company name}"),
		MakeCol("id", "c", "type", "formula", "formula", "{a}"),
		MakeCol("id", "d", "type", "formula", "formula", "{Gone}"),
	}
	after := []Col{
		MakeCol("id", "a", "name", "Org", "type", "lead_field"),
		before[1], before[2], before[3],
	}
	if got, want := BrokenRenames(before, after), []int{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := BrokenRenames(before, before); len(got) != 0 {
		t.Fatalf("no rename, got %v", got)
	}
}
