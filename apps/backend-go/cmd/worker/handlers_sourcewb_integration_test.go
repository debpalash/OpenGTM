//go:build integration

package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcewb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const swWS = "ws-swk-worker"

type swPeople struct{}

func (swPeople) FindPeopleByTitles(_ context.Context, q sourcewb.PeopleQuery) (sourcewb.PeopleResult, error) {
	return sourcewb.PeopleResult{People: []sourcewb.Person{{Name: "Ann Lee", Title: "CTO", LinkedIn: "https://linkedin.com/in/annlee"}},
		SearchesUsed: 1}, nil
}

func swSetup(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	ctx := context.Background()
	clean := func() {
		for _, tb := range []string{"jobs", "person_employments", "person_identifiers", "person_entities", "workbook_rows", "workbooks"} {
			_, _ = owner.Exec(context.Background(), "DELETE FROM "+tb+" WHERE workspace_id = $1", swWS)
		}
	}
	clean()
	t.Cleanup(clean)
	if _, err := owner.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id, status, source_config, columns_config, total_rows)
		VALUES ('wb-swk', 'W', $1, 'draft', '{}', $2, 0)`, swWS,
		`[{"id":"src_p","type":"source","kind":"people_search","companies":["Acme"],"titles":["CTO"]},
		  {"id":"src_c","type":"source","icp":{"description":"dentists"}}]`); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"src_p", "src_c"} {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := queue.EnqueueOnce(ctx, tx, sourcewb.JobType, swWS, map[string]any{"workbook_id": "wb-swk",
			"column_id": col, "workspace_id": swWS}, "swk:"+col, "go"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return pool, owner
}

func swDrain(t *testing.T, pool *pgxpool.Pool, env map[string]string) {
	t.Helper()
	ctx := context.Background()
	log := telemetry.NewLogger(io.Discard)
	cfg := config.Config{HeartbeatInterval: time.Hour, JobTimeout: time.Minute, AppEnv: "test"}
	rn := dispatch(sourcewbHandlers(pool, log, nil, nil, nil, sourcewbOpts{people: swPeople{},
		getenv: func(k string) string { return env[k] }}))
	cctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	done := make(chan struct{})
	go func() { slotLoop(cctx, ctx, pool, cfg, log, "w", rn); close(done) }()
	for cctx.Err() == nil {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE executor='go' AND status IN ('pending','processing')`).Scan(&n)
		if n == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	stop()
	<-done
}

func swJobs(t *testing.T, owner *pgxpool.Pool) map[string][2]string {
	t.Helper()
	rows, err := owner.Query(context.Background(), `SELECT payload->>'column_id', executor, status FROM jobs WHERE workspace_id = $1`, swWS)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var col, ex, st string
		if err := rows.Scan(&col, &ex, &st); err != nil {
			t.Fatal(err)
		}
		out[col] = [2]string{ex, st}
	}
	return out
}

func TestSourcewbFlagOffHandsBack(t *testing.T) {
	pool, owner := swSetup(t)
	swDrain(t, pool, map[string]string{})
	for col, j := range swJobs(t, owner) {
		if j[0] != "python" || j[1] != "pending" {
			t.Errorf("%s: %v, want handed back to python", col, j)
		}
	}
}

func TestSourcewbRunsPeopleAndHandsBackCollection(t *testing.T) {
	pool, owner := swSetup(t)
	swDrain(t, pool, map[string]string{"GO_EXECUTOR_SOURCE_WORKBOOK": "true", "PEOPLE_SEARCH_SOURCE_ENABLED": "true"})
	jobs := swJobs(t, owner)
	if jobs["src_p"] != [2]string{"go", "completed"} {
		t.Errorf("people_search job: %v", jobs["src_p"])
	}
	// internal/collect is not wired: the collection query goes back to Python untouched.
	if jobs["src_c"] != [2]string{"python", "pending"} {
		t.Errorf("collection job: %v", jobs["src_c"])
	}
	var n int
	var status string
	if err := owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-swk'), status
		FROM workbooks WHERE id='wb-swk'`).Scan(&n, &status); err != nil {
		t.Fatal(err)
	}
	if n != 1 || status != "draft" {
		t.Errorf("rows=%d status=%s", n, status)
	}
}
