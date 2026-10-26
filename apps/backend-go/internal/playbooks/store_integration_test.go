//go:build integration

package playbooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govaudit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool    *pgxpool.Pool
	ws, ws2 string
	aud     string // audience in ws
	aud2    string // audience in ws2
}

var ctr int

func setup(t *testing.T) *env {
	t.Helper()
	p := testpg.NewAppPool(t, 4)
	ctr++
	tag := fmt.Sprintf("pb%d-%d", ctr, os.Getpid())
	e := &env{pool: p, ws: "ws-" + tag, ws2: "ws-" + tag + "-b", aud: "aud-" + tag, aud2: "aud-" + tag + "-b"}
	ctx := context.Background()
	for _, w := range []string{e.ws, e.ws2} {
		if _, err := p.Exec(ctx, `INSERT INTO workspaces (id,name,slug,description,icon,created_at,updated_at) VALUES ($1::text,$1::text,$1::text,'','',1,1)`, w); err != nil {
			t.Fatal(err)
		}
	}
	for ws, a := range map[string]string{e.ws: e.aud, e.ws2: e.aud2} {
		e.in(t, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO audiences (id, workspace_id, name, filters) VALUES ($1,$2,$1,'{}')`, a, ws)
			return err
		})
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, w := range []string{e.ws, e.ws2} {
			_ = tenancy.InWorkspace(c, p, w, func(tx pgx.Tx) error {
				for _, q := range []string{`DELETE FROM playbook_results`, `DELETE FROM playbook_runs`, `DELETE FROM research_playbooks`, `DELETE FROM audiences`} {
					if _, err := tx.Exec(c, q+` WHERE workspace_id=$1`, w); err != nil {
						return err
					}
				}
				return nil
			})
			_, _ = p.Exec(c, `DELETE FROM playbook_schedules WHERE workspace_id=$1`, w)
			_, _ = p.Exec(c, `DELETE FROM jobs WHERE workspace_id=$1`, w)
			_, _ = p.Exec(c, `DELETE FROM workspaces WHERE id=$1`, w)
		}
	})
	return e
}

func (e *env) in(t *testing.T, ws string, fn func(pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), e.pool, ws, fn); err != nil {
		t.Fatalf("in %s: %v", ws, err)
	}
}

func (e *env) do(t *testing.T, ws string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	return tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error { return fn(context.Background(), tx) })
}

func (e *env) mkPlaybook(t *testing.T, ws, name string) Playbook {
	t.Helper()
	var pb Playbook
	if err := e.do(t, ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pb, err = Create(ctx, tx, ws, CreateInput{Name: name, PromptTemplate: "prompt template text", OutputFormat: "text", MaxSteps: 4, CellBudgetUSD: 0.1,
			Steps: []Step{{Key: "s1", Name: "S1", PromptTemplate: "step one prompt", OutputFormat: "text"}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pb
}

func (e *env) mkRun(t *testing.T, ws string, pb Playbook, aud string) Run {
	t.Helper()
	var r Run
	if err := e.do(t, ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = StartRun(ctx, tx, ws, pb, aud, 100, "7")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) sql(t *testing.T, ws, q string, args ...any) {
	t.Helper()
	e.in(t, ws, func(tx pgx.Tx) error { _, err := tx.Exec(context.Background(), q, args...); return err })
}

func sp[T any](v T) *T { return &v }

func TestCreateDuplicateNameConflict(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "dup")
	if pb.Version != 1 || !pb.Enabled || pb.ID == "" || len(pb.Steps) != 1 || pb.NextRunAt != nil || pb.CreatedAt.IsZero() {
		t.Fatalf("created: %+v", pb)
	}
	err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Create(ctx, tx, e.ws, CreateInput{Name: "dup", PromptTemplate: "prompt template text", OutputFormat: "text", MaxSteps: 4, CellBudgetUSD: 0.1})
		return err
	})
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("want ErrNameTaken, got %v", err)
	}
	// another workspace may reuse the name.
	e.mkPlaybook(t, e.ws2, "dup")
	// bad top-level output_format is a router-level 422 in Python.
	err = e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Create(ctx, tx, e.ws, CreateInput{Name: "x", PromptTemplate: "prompt template text", OutputFormat: "xml", MaxSteps: 4, CellBudgetUSD: 0.1})
		return err
	})
	if !errors.Is(err, ErrBadOutputFormat) {
		t.Fatalf("want ErrBadOutputFormat, got %v", err)
	}
	// the tx stays usable after the duplicate (no aborted-transaction state).
	err = e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := Create(ctx, tx, e.ws, CreateInput{Name: "dup", PromptTemplate: "prompt template text", OutputFormat: "text", MaxSteps: 4, CellBudgetUSD: 0.1}); !errors.Is(err, ErrNameTaken) {
			return fmt.Errorf("first: %v", err)
		}
		_, err := List(ctx, tx, e.ws)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListOrderUpdatedAtDesc(t *testing.T) {
	e := setup(t)
	a, b := e.mkPlaybook(t, e.ws, "a"), e.mkPlaybook(t, e.ws, "b")
	e.sql(t, e.ws, `UPDATE research_playbooks SET updated_at = '2020-01-01' WHERE id=$1`, b.ID)
	var got []Playbook
	e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error { var err error; got, err = List(ctx, tx, e.ws); return err })
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("order %+v", got)
	}
}

func TestPatchRenameCollisionIsErrNameTaken(t *testing.T) {
	e := setup(t)
	e.mkPlaybook(t, e.ws, "one")
	two := e.mkPlaybook(t, e.ws, "two")
	err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Patch(ctx, tx, e.ws, two.ID, PatchInput{Name: sp("one")})
		return err
	})
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("want ErrNameTaken (Python: 500), got %v", err)
	}
	// renaming to its own name is not a collision.
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Patch(ctx, tx, e.ws, two.ID, PatchInput{Name: sp("two")})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPatchVersionBumpOnlyOnPromptFields(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "v")
	patch := func(in PatchInput) Playbook {
		var out Playbook
		if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = Patch(ctx, tx, e.ws, pb.ID, in)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	ver := pb.Version
	for name, in := range map[string]PatchInput{
		"name": {Name: sp("v2")}, "description": {Description: sp("d")}, "enabled": {Enabled: sp(false)}, "empty": {},
	} {
		if got := patch(in); got.Version != ver {
			t.Fatalf("%s bumped version to %d", name, got.Version)
		}
	}
	steps := []Step{}
	for name, in := range map[string]PatchInput{
		"prompt_template": {PromptTemplate: sp("another prompt text")}, "steps": {Steps: &steps}, "output_format": {OutputFormat: sp("json")},
		"max_steps": {MaxSteps: sp(2)}, "cell_budget_usd": {CellBudgetUSD: sp(1.5)},
	} {
		got := patch(in)
		ver++
		if got.Version != ver {
			t.Fatalf("%s: version %d want %d", name, got.Version, ver)
		}
	}
	// returned row equals a fresh Get and carries the changes.
	var got Playbook
	e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = Get(ctx, tx, e.ws, pb.ID)
		return err
	})
	if got.Name != "v2" || got.Description != "d" || got.Enabled || got.OutputFormat != "json" || got.MaxSteps != 2 || got.CellBudgetUSD != 1.5 || len(got.Steps) != 0 {
		t.Fatalf("persisted %+v", got)
	}
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Patch(ctx, tx, e.ws, pb.ID, PatchInput{OutputFormat: sp("xml")})
		return err
	}); !errors.Is(err, ErrBadOutputFormat) {
		t.Fatalf("want ErrBadOutputFormat, got %v", err)
	}
}

func TestPatchForeignAudienceNotFound(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "aud")
	for _, id := range []string{e.aud2, "no-such-audience"} {
		err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
			_, err := Patch(ctx, tx, e.ws, pb.ID, PatchInput{ScheduleAudienceSet: true, ScheduleAudienceID: sp(id)})
			return err
		})
		if !errors.Is(err, ErrAudienceNotFound) {
			t.Fatalf("%s: want ErrAudienceNotFound, got %v", id, err)
		}
	}
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Patch(ctx, tx, e.ws, "missing", PatchInput{})
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// own audience is fine.
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Patch(ctx, tx, e.ws, pb.ID, PatchInput{ScheduleAudienceSet: true, ScheduleAudienceID: sp(e.aud)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPatchClearsScheduleWithNull(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "sched")
	var got Playbook
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = Patch(ctx, tx, e.ws, pb.ID, PatchInput{ScheduleAudienceSet: true, ScheduleAudienceID: sp(e.aud), ScheduleIntervalSet: true, ScheduleIntervalMinutes: sp(30)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.NextRunAt == nil || got.ScheduleAudienceID == nil || *got.ScheduleIntervalMinutes != 30 {
		t.Fatalf("schedule not set: %+v", got)
	}
	if d := time.Until(got.NextRunAt.UTC()); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("next_run_at %v is not ~30 minutes out (naive UTC)", got.NextRunAt)
	}
	count := func(q string, args ...any) (n int) {
		if err := e.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var mirrorOn bool
	if err := e.pool.QueryRow(context.Background(), `SELECT enabled FROM playbook_schedules WHERE playbook_id=$1`, pb.ID).Scan(&mirrorOn); err != nil || !mirrorOn {
		t.Fatalf("mirror: %v %v", mirrorOn, err)
	}
	wantKey := "playbook_schedule:" + pb.ID + ":" + got.NextRunAt.UTC().Format("2006-01-02T15:04:05.000000") + "+00:00"
	if got.NextRunAt.Nanosecond() == 0 {
		wantKey = "playbook_schedule:" + pb.ID + ":" + got.NextRunAt.UTC().Format("2006-01-02T15:04:05") + "+00:00"
	}
	if n := count(`SELECT count(*) FROM jobs WHERE type='research_playbook_schedule' AND status='pending' AND fire_key=$1 AND workspace_id=$2`, wantKey, e.ws); n != 1 {
		t.Fatalf("pending schedule job with fire_key %s: %d", wantKey, n)
	}
	// disabling (enabled=false) deactivates without touching the audience.
	// null/null clears the schedule.
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = Patch(ctx, tx, e.ws, pb.ID, PatchInput{ScheduleAudienceSet: true, ScheduleIntervalSet: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.ScheduleAudienceID != nil || got.ScheduleIntervalMinutes != nil || got.NextRunAt != nil {
		t.Fatalf("not cleared: %+v", got)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT enabled FROM playbook_schedules WHERE playbook_id=$1`, pb.ID).Scan(&mirrorOn); err != nil || mirrorOn {
		t.Fatalf("mirror should be disabled: %v %v", mirrorOn, err)
	}
	if n := count(`SELECT count(*) FROM jobs WHERE type='research_playbook_schedule' AND status='pending' AND workspace_id=$1`, e.ws); n != 0 {
		t.Fatalf("pending schedule jobs left: %d", n)
	}
	if n := count(`SELECT count(*) FROM jobs WHERE type='research_playbook_schedule' AND status='cancelled' AND workspace_id=$1`, e.ws); n != 1 {
		t.Fatalf("cancelled schedule jobs: %d", n)
	}
}

func TestDeleteBlockedByActiveRun(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "del")
	run := e.mkRun(t, e.ws, pb, e.aud)
	del := func() error {
		return e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error { return Delete(ctx, tx, e.ws, pb.ID) })
	}
	if err := del(); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("pending run: want ErrActiveRun, got %v", err)
	}
	e.sql(t, e.ws, `UPDATE playbook_runs SET status='running' WHERE id=$1`, run.ID)
	if err := del(); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("running run: want ErrActiveRun, got %v", err)
	}
	e.sql(t, e.ws, `UPDATE playbook_runs SET status='completed' WHERE id=$1`, run.ID)
	if err := del(); err != nil {
		t.Fatal(err)
	}
	if err := del(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM playbook_schedules WHERE playbook_id=$1`, pb.ID).Scan(&n)
	if n != 0 {
		t.Fatal("schedule mirror survived delete")
	}
	e.in(t, e.ws, func(tx pgx.Tx) error {
		tx.QueryRow(context.Background(), `SELECT count(*) FROM playbook_runs WHERE playbook_id=$1`, pb.ID).Scan(&n)
		return nil
	})
	if n != 0 {
		t.Fatal("runs survived delete (cascade)")
	}
}

func TestStartRunSnapshotsStepsAndVersion(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "snap")
	e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pb, err = Patch(ctx, tx, e.ws, pb.ID, PatchInput{PromptTemplate: sp("version two prompt")})
		return err
	})
	run := e.mkRun(t, e.ws, pb, e.aud)
	if run.Status != "pending" || run.PromptVersion != 2 || run.MaxMembers != 100 || run.PlaybookID != pb.ID || run.AudienceID != e.aud ||
		run.Attempted != 0 || run.Error != nil || run.StartedAt != nil || run.FinishedAt != nil || run.ID == "" {
		t.Fatalf("run %+v", run)
	}
	// later edits do not change the snapshot.
	e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Patch(ctx, tx, e.ws, pb.ID, PatchInput{PromptTemplate: sp("version three prompt"), Steps: &[]Step{}})
		return err
	})
	var prompt, steps, by string
	var ver int
	e.in(t, e.ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT prompt_snapshot, steps_snapshot::text, requested_by, prompt_version FROM playbook_runs WHERE id=$1`, run.ID).Scan(&prompt, &steps, &by, &ver)
	})
	if prompt != "version two prompt" || ver != 2 || by != "7" {
		t.Fatalf("snapshot %q v%d by %q", prompt, ver, by)
	}
	var js []Step
	if err := decodeSteps([]byte(steps), &js); err != nil || len(js) != 1 || js[0].Key != "s1" || js[0].OutputFormat != "text" {
		t.Fatalf("steps snapshot %s: %v", steps, err)
	}
	// disabled playbook and foreign audience are refused.
	e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pb, err = Patch(ctx, tx, e.ws, pb.ID, PatchInput{Enabled: sp(false)})
		return err
	})
	err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := StartRun(ctx, tx, e.ws, pb, e.aud, 10, "7")
		return err
	})
	if !errors.Is(err, ErrPlaybookDisabled) {
		t.Fatalf("want ErrPlaybookDisabled, got %v", err)
	}
	e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pb, err = Patch(ctx, tx, e.ws, pb.ID, PatchInput{Enabled: sp(true)})
		return err
	})
	err = e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := StartRun(ctx, tx, e.ws, pb, e.aud2, 10, "7")
		return err
	})
	if !errors.Is(err, ErrAudienceNotFound) {
		t.Fatalf("want ErrAudienceNotFound, got %v", err)
	}
}

func TestListRunsCursorAndOffset(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "runs")
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, e.mkRun(t, e.ws, pb, e.aud).ID)
	}
	// runs 0..4: distinct created_at except 3 and 4 which tie (id breaks the tie, desc).
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for i, id := range ids {
		ts := base.Add(time.Duration(i) * time.Minute)
		if i == 4 {
			ts = base.Add(3 * time.Minute)
		}
		e.sql(t, e.ws, `UPDATE playbook_runs SET created_at=$1 WHERE id=$2`, ts, id)
	}
	list := func(limit, offset int, cursor string) (RunPage, error) {
		var pg RunPage
		err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			pg, err = ListRuns(ctx, tx, e.ws, pb.ID, limit, offset, cursor)
			return err
		})
		return pg, err
	}
	// expected total order: created_at desc, id desc.
	want := []string{}
	tie := []string{ids[3], ids[4]}
	if tie[0] < tie[1] {
		tie[0], tie[1] = tie[1], tie[0]
	}
	want = append(want, tie...)
	want = append(want, ids[2], ids[1], ids[0])

	var got []string
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		pg, err := list(2, 0, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pg.Runs {
			got = append(got, r.ID)
		}
		if cursor != "" && pg.Offset != nil {
			t.Fatalf("offset must be null with a cursor, got %d", *pg.Offset)
		}
		if cursor == "" && (pg.Offset == nil || *pg.Offset != 0) {
			t.Fatalf("first page offset %v", pg.Offset)
		}
		if !pg.HasMore {
			if pg.NextCursor != nil {
				t.Fatal("next_cursor on last page")
			}
			break
		}
		if pg.NextCursor == nil {
			t.Fatal("missing next_cursor")
		}
		cursor = *pg.NextCursor
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("cursor walk %v want %v", got, want)
	}
	// offset paging (no cursor).
	pg, err := list(2, 2, "")
	if err != nil || len(pg.Runs) != 2 || pg.Runs[0].ID != want[2] || pg.Offset == nil || *pg.Offset != 2 || !pg.HasMore {
		t.Fatalf("offset page %+v %v", pg, err)
	}
	// offset is ignored when a cursor is present.
	first, _ := list(1, 0, "")
	pg, err = list(1, 3, *first.NextCursor)
	if err != nil || pg.Runs[0].ID != want[1] {
		t.Fatalf("cursor+offset: %+v %v", pg, err)
	}
	// limit and offset are clamped.
	pg, _ = list(10000, 0, "")
	if pg.Limit != 200 {
		t.Fatalf("limit not capped: %d", pg.Limit)
	}
	pg, _ = list(0, -5, "")
	if pg.Limit != 1 || *pg.Offset != 0 {
		t.Fatalf("lower clamp: %d %v", pg.Limit, pg.Offset)
	}
	// malformed cursor -> 422 CursorError; unknown playbook -> ErrNotFound.
	_, err = list(2, 0, "!!!notbase64")
	var ce *govaudit.CursorError
	if !errors.As(err, &ce) || ce.Detail != "invalid playbook run cursor" {
		t.Fatalf("want playbook run cursor error, got %v", err)
	}
	err = e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ListRuns(ctx, tx, e.ws, "nope", 10, 0, "")
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func (e *env) mkResults(t *testing.T, ws, runID string, statuses ...string) {
	t.Helper()
	for i, s := range statuses {
		e.sql(t, ws, `INSERT INTO playbook_results (workspace_id, run_id, lead_id, status, value, result_metadata, attempts) VALUES ($1,$2,$3,$4,'v','{"k": 1.50}',1)`, ws, runID, i+1, s)
	}
}

func TestListResultsCursorMalformed422(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "res")
	run := e.mkRun(t, e.ws, pb, e.aud)
	e.mkResults(t, e.ws, run.ID, "success", "failed", "success", "pending", "success")
	list := func(limit, offset int, cursor string) (ResultPage, error) {
		var pg ResultPage
		err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			pg, err = ListResults(ctx, tx, e.ws, run.ID, limit, offset, cursor)
			return err
		})
		return pg, err
	}
	for _, bad := range []string{"abc", "-1", "1.5", "0x10", "--1"} {
		_, err := list(10, 0, bad)
		var ce *govaudit.CursorError
		if !errors.As(err, &ce) || ce.Detail != "invalid playbook result cursor" || ce.Status() != 422 {
			t.Fatalf("%q: want 422 invalid playbook result cursor, got %v", bad, err)
		}
	}
	pg, err := list(2, 0, "")
	if err != nil || len(pg.Results) != 2 || !pg.HasMore || pg.NextCursor == nil || pg.Results[0].ID >= pg.Results[1].ID {
		t.Fatalf("page1 %+v %v", pg, err)
	}
	if *pg.NextCursor != strconv.Itoa(int(pg.Results[1].ID)) {
		t.Fatalf("next_cursor %s != last id %d", *pg.NextCursor, pg.Results[1].ID)
	}
	if string(pg.Results[0].Metadata) == "" {
		t.Fatal("metadata missing")
	}
	pg2, err := list(2, 99, *pg.NextCursor) // offset ignored under cursor
	if err != nil || len(pg2.Results) != 2 || pg2.Results[0].ID <= pg.Results[1].ID || pg2.Offset != nil {
		t.Fatalf("page2 %+v %v", pg2, err)
	}
	pg3, _ := list(2, 0, *pg2.NextCursor)
	if len(pg3.Results) != 1 || pg3.HasMore || pg3.NextCursor != nil {
		t.Fatalf("page3 %+v", pg3)
	}
	// " 3 " parses (Python int()); a cursor past every id yields an empty page.
	if _, err := list(2, 0, " 1 "); err != nil {
		t.Fatalf("whitespace int cursor: %v", err)
	}
	pg, _ = list(2, 0, "999999999")
	if len(pg.Results) != 0 || pg.HasMore {
		t.Fatalf("past-end page %+v", pg)
	}
	pg, _ = list(5000, 0, "")
	if pg.Limit != 500 {
		t.Fatalf("limit cap %d", pg.Limit)
	}
	err = e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ListResults(ctx, tx, e.ws, "nope", 10, 0, "")
		return err
	})
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

func TestRetryOnlyTerminalFailed(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "retry")
	retry := func(id string) (Run, error) {
		var r Run
		err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = RetryRun(ctx, tx, e.ws, id)
			return err
		})
		return r, err
	}
	for _, st := range []string{"failed", "completed_with_errors", "cancelled"} {
		run := e.mkRun(t, e.ws, pb, e.aud)
		e.sql(t, e.ws, `UPDATE playbook_runs SET status=$2, error='x', finished_at=now(), failed=3 WHERE id=$1`, run.ID, st)
		got, err := retry(run.ID)
		if err != nil || got.Status != "pending" || got.Error != nil || got.FinishedAt != nil || got.Failed != 3 {
			t.Fatalf("%s: %+v %v", st, got, err)
		}
	}
	for _, st := range []string{"pending", "running", "completed"} {
		run := e.mkRun(t, e.ws, pb, e.aud)
		e.sql(t, e.ws, `UPDATE playbook_runs SET status=$2 WHERE id=$1`, run.ID, st)
		if _, err := retry(run.ID); !errors.Is(err, ErrNotRetryable) {
			t.Fatalf("%s: want ErrNotRetryable, got %v", st, err)
		}
	}
	if _, err := retry("missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

func TestCancelOnlyActive(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "cancel")
	cancel := func(id string) (Run, error) {
		var r Run
		err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = CancelRun(ctx, tx, e.ws, id)
			return err
		})
		return r, err
	}
	for _, st := range []string{"pending", "running"} {
		run := e.mkRun(t, e.ws, pb, e.aud)
		e.sql(t, e.ws, `UPDATE playbook_runs SET status=$2 WHERE id=$1`, run.ID, st)
		if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error { return EnqueueRun(ctx, tx, e.ws, run.ID, "python") }); err != nil {
			t.Fatal(err)
		}
		got, err := cancel(run.ID)
		if err != nil || got.Status != "cancelled" || got.Error == nil || *got.Error != "Cancelled by user" || got.FinishedAt == nil {
			t.Fatalf("%s: %+v %v", st, got, err)
		}
		var js string
		if err := e.pool.QueryRow(context.Background(), `SELECT status||'|'||coalesce(error,'') FROM jobs WHERE fire_key=$1 AND workspace_id=$2`, "playbook:"+run.ID, e.ws).Scan(&js); err != nil || js != "cancelled|Cancelled by user" {
			t.Fatalf("job after cancel: %q %v", js, err)
		}
	}
	for _, st := range []string{"completed", "failed", "cancelled", "completed_with_errors"} {
		run := e.mkRun(t, e.ws, pb, e.aud)
		e.sql(t, e.ws, `UPDATE playbook_runs SET status=$2 WHERE id=$1`, run.ID, st)
		if _, err := cancel(run.ID); !errors.Is(err, ErrNotCancellable) {
			t.Fatalf("%s: want ErrNotCancellable, got %v", st, err)
		}
	}
	if _, err := cancel("missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

func TestEnqueueRunPayloadAndExecutor(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "enq")
	run := e.mkRun(t, e.ws, pb, e.aud)
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error { return EnqueueRun(ctx, tx, e.ws, run.ID, "go") }); err != nil {
		t.Fatal(err)
	}
	var ex, typ, pl, st, ws string
	var pr int
	if err := e.pool.QueryRow(context.Background(), `SELECT executor, type, payload::text, priority, status, workspace_id FROM jobs WHERE fire_key=$1`, "playbook:"+run.ID).Scan(&ex, &typ, &pl, &pr, &st, &ws); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"workspace_id":"%s","run_id":"%s"}`, e.ws, run.ID)
	if ex != "go" || typ != "research_playbook_run" || pl != want || pr != 1 || st != "pending" || ws != e.ws {
		t.Fatalf("job %s %s %s %d %s %s", ex, typ, pl, pr, st, ws)
	}
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error { return EnqueueRun(ctx, tx, e.ws, run.ID, "rust") }); err == nil {
		t.Fatal("unknown executor accepted")
	}
}

func TestResultCountsFromLedger(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "counts")
	run := e.mkRun(t, e.ws, pb, e.aud)
	count := func() (a, s, f int) {
		e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error { a, s, f = ResultCounts(ctx, tx, e.ws, run.ID); return nil })
		return
	}
	if a, s, f := count(); a != 0 || s != 0 || f != 0 {
		t.Fatalf("empty ledger: %d %d %d", a, s, f)
	}
	e.mkResults(t, e.ws, run.ID, "success", "success", "failed", "pending", "running")
	if a, s, f := count(); a != 5 || s != 2 || f != 1 {
		t.Fatalf("counts %d %d %d, want 5 2 1", a, s, f)
	}
}

func TestStoreTenantIsolation(t *testing.T) {
	e := setup(t)
	pb := e.mkPlaybook(t, e.ws, "iso")
	run := e.mkRun(t, e.ws, pb, e.aud)
	e.mkResults(t, e.ws, run.ID, "success")
	e.sql(t, e.ws, `UPDATE playbook_runs SET status='failed' WHERE id=$1`, run.ID)
	other := e.mkPlaybook(t, e.ws2, "iso") // same name in the other workspace is fine
	cases := map[string]func(ctx context.Context, tx pgx.Tx) error{
		"get": func(ctx context.Context, tx pgx.Tx) error { _, err := Get(ctx, tx, e.ws2, pb.ID); return err },
		"patch": func(ctx context.Context, tx pgx.Tx) error {
			_, err := Patch(ctx, tx, e.ws2, pb.ID, PatchInput{Name: sp("x")})
			return err
		},
		"delete": func(ctx context.Context, tx pgx.Tx) error { return Delete(ctx, tx, e.ws2, pb.ID) },
		"runs": func(ctx context.Context, tx pgx.Tx) error {
			_, err := ListRuns(ctx, tx, e.ws2, pb.ID, 10, 0, "")
			return err
		},
	}
	for name, fn := range cases {
		if err := e.do(t, e.ws2, fn); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", name, err)
		}
	}
	runCases := map[string]func(ctx context.Context, tx pgx.Tx) error{
		"results": func(ctx context.Context, tx pgx.Tx) error {
			_, err := ListResults(ctx, tx, e.ws2, run.ID, 10, 0, "")
			return err
		},
		"retry":  func(ctx context.Context, tx pgx.Tx) error { _, err := RetryRun(ctx, tx, e.ws2, run.ID); return err },
		"cancel": func(ctx context.Context, tx pgx.Tx) error { _, err := CancelRun(ctx, tx, e.ws2, run.ID); return err },
	}
	for name, fn := range runCases {
		if err := e.do(t, e.ws2, fn); !errors.Is(err, ErrRunNotFound) {
			t.Errorf("%s: want ErrRunNotFound, got %v", name, err)
		}
	}
	// start a run against ws's playbook value from ws2 must not create a row visible anywhere in ws.
	err := e.do(t, e.ws2, func(ctx context.Context, tx pgx.Tx) error {
		_, err := StartRun(ctx, tx, e.ws2, pb, e.aud2, 1, "1")
		return err
	})
	if err == nil {
		t.Error("StartRun with a foreign playbook value succeeded")
	}
	e.do(t, e.ws2, func(ctx context.Context, tx pgx.Tx) error {
		if a, s, f := ResultCounts(ctx, tx, e.ws2, run.ID); a+s+f != 0 {
			t.Errorf("foreign counts %d %d %d", a, s, f)
		}
		l, _ := List(ctx, tx, e.ws2)
		if len(l) != 1 || l[0].ID != other.ID {
			t.Errorf("list leaked: %+v", l)
		}
		return nil
	})
	// ws's data untouched.
	e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error {
		got, err := Get(ctx, tx, e.ws, pb.ID)
		if err != nil || got.Name != "iso" {
			t.Errorf("ws playbook changed: %+v %v", got, err)
		}
		return nil
	})
}
