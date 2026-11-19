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
		{"alembic at v3.0.0 head", dbState{gooseVersion: -1, alembicTable: true, alembic: []string{BridgeFromRevision}, tables: 69}, planBridge, ""},
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

func TestRefusalNamesBothSupportedStartingPoints(t *testing.T) {
	_, err := decide(dbState{gooseVersion: -1, alembicTable: true, alembic: []string{"d3f5a7b9c1e2"}, tables: 90})
	if err == nil {
		t.Fatal("expected refusal")
	}
	for _, want := range []string{"d3f5a7b9c1e2", BaselineAlembicRevision, BridgeFromRevision, "v3.0.0"} {
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
	// Final correctness I6: the startup gate is always the latest migration (add a migration, bump the constant).
	if config.RequiredGoSchemaVersion != v {
		t.Fatalf("config.RequiredGoSchemaVersion %d != latest embedded migration %d", config.RequiredGoSchemaVersion, v)
	}
	if config.RequiredGoSchemaRevision != BaselineAlembicRevision {
		t.Fatalf("config.RequiredGoSchemaRevision %q != baseline Alembic revision %q", config.RequiredGoSchemaRevision, BaselineAlembicRevision)
	}
}

func sqlBody(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "--") {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

func TestBridgeSQL(t *testing.T) {
	bridgeDDL := sqlBody(bridgeDDL)
	bridgeJobs := sqlBody(bridgeJobs)
	if strings.Contains(bridgeDDL, "CONCURRENTLY") || strings.Contains(bridgeDDL, "BEGIN;") || strings.Contains(bridgeDDL, "alembic_version") {
		t.Error("bridge DDL must be transaction-safe and leave the stamp to bridge.go")
	}
	if strings.Contains(bridgeDDL, "yupcha_app") || !strings.Contains(bridgeDDL, "{{APP_ROLE}}") {
		t.Error("bridge DDL must use the {{APP_ROLE}} placeholder, not a hardcoded role")
	}
	for _, want := range []string{"ADD COLUMN executor", "CREATE TABLE workspaces", "CREATE TABLE collection_jobs",
		"outreach_send_by_message_id", "ix_outreach_sends_message_id", "destination_runs ALTER COLUMN status TYPE VARCHAR(32)"} {
		if !strings.Contains(bridgeDDL, want) {
			t.Errorf("bridge DDL lacks %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(bridgeJobs), "DELETE") || strings.Contains(strings.ToUpper(bridgeJobs), "DROP") {
		t.Error("the bridge jobs step must never delete rows")
	}
}
