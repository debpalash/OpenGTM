package records

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInspectImportKeepsDuplicateSuggestionsEditable(t *testing.T) {
	request := ImportRequest{Kind: Company, Source: "clay:table", CSV: "Company Name,Name,Row ID\nAtlas,Atlas Labs,0000000000000123\n"}
	inspection, err := InspectImport(request)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Rows != 1 || len(inspection.Columns) != 3 || inspection.Columns[0].Name != "Company Name" {
		t.Fatal("source order or counts changed")
	}
	if inspection.Columns[0].Target != "company.name" || inspection.Columns[1].Target != "company.name" {
		t.Fatal("ambiguous header suggestions were silently discarded")
	}
	if inspection.Columns[2].Samples[0].Value != "0000000000000123" || !inspection.Columns[2].PrecisionSensitive {
		t.Fatal("identifier precision/leading zeros changed")
	}
	if _, _, err := request.prepare(); err == nil {
		t.Fatal("row validation accepted conflicting targets")
	}
	request.Mapping = map[string]string{"Name": ""}
	if _, _, err := request.prepare(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectImportSamplesPreserveNumbersAndDoNotRewritePayload(t *testing.T) {
	request := ImportRequest{Kind: Person, Source: "clay:people", Rows: []map[string]any{
		{"Row ID": json.Number("9007199254740993"), "Job Title": "Founder", "Account notes": map[string]any{"id": json.Number("9007199254740995")}},
		{"Row ID": json.Number("9007199254740994"), "Job Title": "Researcher"},
	}}
	before, _ := json.Marshal(request)
	inspection, err := InspectImport(request)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ImportColumn{}
	for _, column := range inspection.Columns {
		byName[column.Name] = column
	}
	if byName["Row ID"].Samples[0].Value != "9007199254740993" || !byName["Row ID"].PrecisionSensitive || !byName["Account notes"].PrecisionSensitive {
		t.Fatal("numeric source identifiers were rounded or unmarked")
	}
	if byName["Job Title"].Match != "matched" || byName["Job Title"].Target != "employment.title" || byName["Account notes"].Match != "custom" {
		t.Fatal("field ownership or match provenance was lost")
	}
	after, _ := json.Marshal(request)
	if string(before) != string(after) {
		t.Fatal("inspection mutated the source payload")
	}
}

func TestInspectImportSamplesAreBoundedAndUnicodeSafe(t *testing.T) {
	rows := []map[string]any{}
	for i := 0; i < 5; i++ {
		rows = append(rows, map[string]any{"company.notes": strings.Repeat("界", 200)})
	}
	out, err := InspectImport(ImportRequest{Kind: Company, Source: "source", Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	column := out.Columns[0]
	if len(column.Samples) != 3 || len([]rune(column.Samples[0].Value)) != 160 || !column.Samples[0].Truncated || column.Match != "explicit" {
		t.Fatal("sample bounds or explicit field-path provenance changed")
	}
}

func TestInspectImportUsesExistingFileValidation(t *testing.T) {
	for _, csv := range []string{"Name,Name\nA,B\n", "Name,\nA,B\n", "Name,Domain\nA\n", "Name\n"} {
		if _, err := InspectImport(ImportRequest{Kind: Company, Source: "source", CSV: csv}); err == nil {
			t.Errorf("invalid source accepted: %q", csv)
		}
	}
}
