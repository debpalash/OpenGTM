package index

import (
	"os"
	"testing"
)

func TestCommittedExampleIndexMatchesSchema(t *testing.T) {
	doc, err := os.ReadFile("../../../../../docs/plugins/index-example/index.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := schemaErr(t, compileSchema(t), doc); err != nil {
		t.Fatalf("the committed example index violates the schema: %v", err)
	}
	if _, err := Parse(doc); err != nil {
		t.Fatal(err)
	}
}
