package runplan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// The run payload carries the manifest the worker executes; the Go-only extensions
// (response.value_maps, request.omit_empty) must survive it, and must not appear for a manifest
// that does not declare them (so the golden payloads stay byte-equal).
func TestManifestJSONCarriesExtensionsOnlyWhenDeclared(t *testing.T) {
	var plain manifest.Manifest
	if err := json.Unmarshal([]byte(`{"name":"p","capability":"email","request":{"url":"https://a.example/x"},"response":{"mappings":{"email":"$.e"}}}`), &plain); err != nil {
		t.Fatal(err)
	}
	b, err := ManifestJSON(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "value_maps") || strings.Contains(string(b), "omit_empty") {
		t.Fatalf("undeclared extension in payload: %s", b)
	}

	var ext manifest.Manifest
	src := `{"name":"e","capability":"email_verify","request":{"url":"https://a.example/x","omit_empty":["q"]},
	 "response":{"mappings":{"email_verify":"$.s"},"value_maps":{"email_verify":{"values":{"ok":"valid"},"default":"unknown"}}}}`
	if err := json.Unmarshal([]byte(src), &ext); err != nil {
		t.Fatal(err)
	}
	b, err = ManifestJSON(ext)
	if err != nil {
		t.Fatal(err)
	}
	var back manifest.Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Response.ValueMaps, ext.Response.ValueMaps) || !reflect.DeepEqual(back.Request.OmitEmpty, ext.Request.OmitEmpty) {
		t.Fatalf("round trip lost an extension: %s", b)
	}
}
