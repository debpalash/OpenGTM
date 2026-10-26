package govadmin

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// testdata/permissions.json is generated from Python PERMISSIONS by
// testdata/gen_permissions.py.txt (run from legacy/python with PYTHONPATH=.).
func TestCatalogMatchesPythonOrderAndRoles(t *testing.T) {
	raw, err := os.ReadFile("testdata/permissions.json")
	if err != nil {
		t.Fatal(err)
	}
	var want []Permission
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != 11 {
		t.Fatalf("fixture has %d entries", len(want))
	}
	if !reflect.DeepEqual(Permissions, want) {
		t.Fatalf("catalog drift:\n got %+v\nwant %+v", Permissions, want)
	}
	out, _ := json.Marshal(Permissions[0])
	if string(out) != `{"key":"tables.write","label":"Tables","description":"Create, edit and run workbooks","default_roles":["editor","admin"]}` {
		t.Fatalf("json key order: %s", out)
	}
}
