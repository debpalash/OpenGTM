package runplan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
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

// The credential endpoint serves auth.env_var and auth.secondary.env_var and nothing else, so a
// manifest with auth.secondary is eligible, while extra_env (undeclared environment, issue #114)
// stays ineligible exactly as before, with or without a secondary.
func TestEligibilitySecondaryCredentialAndExtraEnv(t *testing.T) {
	cols, err := colgraph.ParseCols([]byte(`[{"id":"e","type":"enrichment","target_field":"description","waterfall":["p"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{WorkspaceID: "w", GoExecutorEnabled: true, GoCohort: map[string]bool{"w": true}}
	mk := func(auth string) map[string]manifest.Entry {
		raw := `{"name":"p","capability":"email","request":{"method":"GET","url":"https://a.example/x"},
		 "response":{"mappings":{"description":"$.e"}},"auth":` + auth + `}`
		e, err := manifest.Decode([]byte(raw), "p")
		if err != nil {
			t.Fatal(err)
		}
		return map[string]manifest.Entry{"p": e}
	}
	sec := `"secondary":{"type":"query","param":"secret","env_var":"P_SECRET"}`
	cases := []struct {
		name, auth string
		ok         bool
		reason     string
	}{
		{"primary only", `{"type":"header","param":"X","env_var":"P_KEY"}`, true, ReasonOK},
		{"primary and secondary", `{"type":"header","param":"X","env_var":"P_KEY",` + sec + `}`, true, ReasonOK},
		{"extra_env stays rejected", `{"type":"header","param":"X","env_var":"P_KEY","extra_env":["P_OTHER"]}`, false, ReasonManifestExtraEnv},
		{"extra_env beside a secondary", `{"type":"header","param":"X","env_var":"P_KEY",` + sec + `,"extra_env":["P_OTHER"]}`, false, ReasonManifestExtraEnv},
		{"denied secondary", `{"type":"header","param":"X","env_var":"P_KEY","secondary":{"type":"query","param":"s","env_var":"SECRET_KEY"}}`, false, "secondary_env_denied"},
	}
	for _, c := range cases {
		ok, why := Eligible(cols, mk(c.auth), cfg)
		if ok != c.ok || why != c.reason {
			t.Errorf("%s: got (%v, %q) want (%v, %q)", c.name, ok, why, c.ok, c.reason)
		}
	}
}

// auth.secondary, response.success_when and poll survive the run payload.
func TestManifestJSONCarriesSecondarySuccessWhenAndPoll(t *testing.T) {
	src := `{"name":"e","capability":"email","auth":{"type":"header","param":"X","env_var":"K",
	  "secondary":{"type":"query","param":"s","env_var":"S"}},
	 "request":{"method":"POST","url":"https://a.example/x"},
	 "response":{"mappings":{"email":"$.e"},"success_when":{"path":"$.ok","equals":true}},
	 "poll":{"request":{"method":"GET","url":"https://a.example/j/{{response.id}}","timeout":5},
	  "interval":2,"max_attempts":4,"max_total":30,
	  "done_when":{"status":[200],"path":"$.state","in":["a","b"]},"failed_when":{"path":"$.state","equals":"x"},
	  "allowed_hosts":["b.example"]}}`
	var m manifest.Manifest
	if err := json.Unmarshal([]byte(src), &m); err != nil {
		t.Fatal(err)
	}
	b, err := ManifestJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	var back manifest.Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if !reflect.DeepEqual(back.Auth.Secondary, m.Auth.Secondary) || !reflect.DeepEqual(back.Response.SuccessWhen, m.Response.SuccessWhen) || !reflect.DeepEqual(back.Poll, m.Poll) {
		t.Fatalf("round trip lost an extension:\n%s\n%+v\n%+v", b, back.Poll, m.Poll)
	}
}
