//go:build integration

package automationhook_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const sigWS = "ws-ah-sig"

func sigSetup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := setup(t)
	clean := func() {
		_ = tenancy.InWorkspace(ctx, pool, sigWS, func(tx pgx.Tx) error {
			for _, q := range []string{`DELETE FROM triggers`, `DELETE FROM workbook_rows`, `DELETE FROM workbooks`} {
				if _, err := tx.Exec(ctx, q); err != nil {
					return err
				}
			}
			return nil
		})
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE type='trigger_eval'`)
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func sigSeed(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	err := tenancy.InWorkspace(ctx, pool, sigWS, func(tx pgx.Tx) error {
		for _, q := range []string{
			`INSERT INTO workbooks (id, name, workspace_id) VALUES ('wb-s1','n','` + sigWS + `'), ('wb-s2','n','` + sigWS + `')`,
			`INSERT INTO workbook_rows (workbook_id, workspace_id, lead_id, data) VALUES ('wb-s1','` + sigWS + `',7,'{}'::json), ('wb-s2','` + sigWS + `',7,'{}'::json), ('wb-s1','` + sigWS + `',8,'{}'::json)`,
			`INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, actions, scope_workbook_ids) VALUES
			   ('r-any',  '` + sigWS + `','n',true, 'on_signal','{}'::json,'[]'::json,'[]'::json),
			   ('r-hire', '` + sigWS + `','n',true, 'on_signal','{"signal_types":["hiring"]}'::json,'[]'::json,'["wb-s1"]'::json),
			   ('r-fund', '` + sigWS + `','n',true, 'on_signal','{"signal_types":["funding"]}'::json,'[]'::json,'[]'::json),
			   ('r-off',  '` + sigWS + `','n',false,'on_signal','{}'::json,'[]'::json,'[]'::json),
			   ('r-rowadd','` + sigWS + `','n',true,'on_row_added','{}'::json,'[]'::json,'[]'::json)`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func lead(n int64) *int64 { return &n }

func TestSignalsInsertedEnqueuesLikePythonOnSignal(t *testing.T) {
	pool, ctx := sigSetup(t)
	sigSeed(t, pool, ctx)
	var n int
	err := tenancy.InWorkspace(ctx, pool, sigWS, func(tx pgx.Tx) error {
		var err error
		n, err = automationhook.New(true, slog.Default()).SignalsInserted(ctx, tx, sigWS, []automationhook.SignalRef{
			{PK: "sig-1", Type: "hiring", LeadID: lead(7)},
			{PK: "sig-2", Type: "hiring", LeadID: nil},      // no lead: skipped
			{PK: "sig-3", Type: "hiring", LeadID: lead(99)}, // lead in no workbook: no firing
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// sig-1: r-any (both workbooks), r-hire (scoped to wb-s1 only). r-fund/off/row-added do not match.
	if n != 2 {
		t.Fatalf("enqueued %d, want 2", n)
	}
	got := jobs(t, pool, ctx)
	if len(got) != 2 {
		t.Fatalf("jobs %+v", got)
	}
	byTrigger := map[string]job{}
	for _, j := range got {
		if j.Executor != "go" || j.Status != "pending" || j.Priority != 1 || j.WorkspaceID != sigWS || j.FireKey != nil && *j.FireKey != "" {
			t.Fatalf("job %+v", j)
		}
		for _, id := range []string{"r-any", "r-hire"} {
			if strings.Contains(j.Payload, `"trigger_id": "`+id+`"`) || strings.Contains(j.Payload, `"trigger_id":"`+id+`"`) {
				byTrigger[id] = j
			}
		}
		for _, want := range []string{`"fire_source"`, `signal`, `"fire_key"`, `signal:sig-1`, `"dry_run"`, `false`} {
			if !strings.Contains(j.Payload, want) {
				t.Fatalf("payload %s lacks %s", j.Payload, want)
			}
		}
	}
	if len(byTrigger) != 2 {
		t.Fatalf("triggers fired: %v", byTrigger)
	}
	if strings.Count(byTrigger["r-any"].Payload, `"row_id"`) != 2 || strings.Count(byTrigger["r-hire"].Payload, `"row_id"`) != 1 {
		t.Fatalf("targets: any=%s hire=%s", byTrigger["r-any"].Payload, byTrigger["r-hire"].Payload)
	}
	// key order mirrors events._enqueue_eval
	p := byTrigger["r-any"].Payload
	last := -1
	for _, k := range []string{"trigger_id", "workspace_id", "targets", "fire_source", "fire_key", "dry_run"} {
		i := strings.Index(p, `"`+k+`"`)
		if i < last {
			t.Fatalf("key order: %s", p)
		}
		last = i
	}
}

func TestSignalsInsertedDisabledAndEmpty(t *testing.T) {
	pool, ctx := sigSetup(t)
	sigSeed(t, pool, ctx)
	for name, h := range map[string]automationhook.Hook{"disabled": automationhook.New(false, slog.Default()), "enabled-empty": automationhook.New(true, slog.Default())} {
		sigs := []automationhook.SignalRef{{PK: "s", Type: "hiring", LeadID: lead(7)}}
		if name == "enabled-empty" {
			sigs = nil
		}
		err := tenancy.InWorkspace(ctx, pool, sigWS, func(tx pgx.Tx) error {
			n, err := h.SignalsInserted(ctx, tx, sigWS, sigs)
			if n != 0 {
				t.Errorf("%s: n=%d", name, n)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := jobs(t, pool, ctx); len(got) != 0 {
		t.Fatalf("jobs %+v", got)
	}
}
