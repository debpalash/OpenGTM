package migrate

import (
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name    string
		st      dbState
		want    plan
		wantErr string
	}{
		{"empty database", dbState{gooseVersion: -1}, planFresh, ""},
		{"goose table but baseline never applied", dbState{gooseVersion: 0}, planFresh, ""},
		{"go-managed", dbState{gooseVersion: 1, alembicTable: true, alembic: []string{BaselineAlembicRevision}}, planGoose, ""},
		{"go-managed ahead", dbState{gooseVersion: 7}, planGoose, ""},
		{"alembic at baseline", dbState{gooseVersion: -1, alembicTable: true, alembic: []string{BaselineAlembicRevision}, tables: 90}, planAdopt, ""},
		{"alembic older", dbState{gooseVersion: -1, alembicTable: true, alembic: []string{"a7c1e9d3b5f2"}, tables: 90}, 0, `Alembic revision "a7c1e9d3b5f2"`},
		{"alembic branches", dbState{gooseVersion: -1, alembicTable: true, alembic: []string{"a", "b"}, tables: 90}, 0, "Alembic revision"},
		{"alembic empty", dbState{gooseVersion: -1, alembicTable: true, tables: 90}, 0, "alembic_version is empty"},
		{"unmanaged tables", dbState{gooseVersion: -1, tables: 3}, 0, "not managed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decide(c.st)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("decide = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

func TestRefusalTellsOperatorToUpgradeWithLegacyTool(t *testing.T) {
	_, err := decide(dbState{gooseVersion: -1, alembicTable: true, alembic: []string{"d3f5a7b9c1e2"}, tables: 90})
	if err == nil {
		t.Fatal("expected refusal")
	}
	for _, want := range []string{"alembic upgrade", BaselineAlembicRevision, "legacy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}

func TestValidRole(t *testing.T) {
	for _, ok := range []string{"yupcha_app", "App_1", "_x"} {
		if err := validRole(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1abc", "a-b", `a"b`, "a b", "x;drop"} {
		if err := validRole(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBaselineSQL(t *testing.T) {
	if !strings.Contains(baselineSQL, "CREATE TABLE public.alembic_version (") {
		t.Error("baseline lacks alembic_version (Python reads the stamp)")
	}
	if !strings.Contains(baselineSQL, "CREATE FUNCTION public.outreach_send_by_message_id(") {
		t.Error("baseline lacks outreach_send_by_message_id")
	}
	// Grants are applied by baseline.go with the configured role, not hardcoded.
	for _, bad := range []string{"yupcha_app", "GRANT ", "REVOKE "} {
		if strings.Contains(baselineSQL, bad) {
			t.Errorf("baseline SQL contains %q", bad)
		}
	}
}

func TestLatestVersionCoversRequired(t *testing.T) {
	v, err := LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v < BaselineVersion {
		t.Fatalf("latest %d < baseline", v)
	}
	if config.RequiredGoSchemaVersion < BaselineVersion || config.RequiredGoSchemaVersion > v {
		t.Fatalf("config.RequiredGoSchemaVersion %d outside [%d, %d]", config.RequiredGoSchemaVersion, BaselineVersion, v)
	}
	if config.RequiredGoSchemaRevision != BaselineAlembicRevision {
		t.Fatalf("config.RequiredGoSchemaRevision %q != baseline Alembic revision %q", config.RequiredGoSchemaRevision, BaselineAlembicRevision)
	}
}
