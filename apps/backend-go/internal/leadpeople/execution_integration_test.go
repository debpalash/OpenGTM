//go:build integration

package leadpeople

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type execStep struct {
	Name string `json:"name"`
	Args struct {
		WorkspaceID    string          `json:"workspace_id"`
		ActionID       string          `json:"action_id"`
		Contract       json.RawMessage `json:"contract"`
		TimeoutSeconds float64         `json:"timeout_seconds"`
	} `json:"args"`
	Op      string         `json:"op"`
	Payload map[string]any `json:"payload"`
	Now     float64        `json:"now"`
	Result  map[string]any `json:"result"`
	Raises  string         `json:"raises"`
	Rows    []struct {
		WorkspaceID  string  `json:"workspace_id"`
		ActionID     string  `json:"action_id"`
		ContractHash string  `json:"contract_hash"`
		Status       string  `json:"status"`
		StartedAt    float64 `json:"started_at"`
		ExpiresAt    float64 `json:"expires_at"`
		Result       *string `json:"result"`
	} `json:"rows"`
}

func execPools(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	app := testpg.NewAppPool(t, 4)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = owner.Exec(context.Background(), `DELETE FROM contact_executions WHERE workspace_id = ANY($1)`, []string{"w1", "w2", "wp-race"})
	}
	clean()
	t.Cleanup(func() { clean(); owner.Close() })
	return app, owner
}

func TestContactExecutionVectors(t *testing.T) {
	app, owner := execPools(t)
	var steps []execStep
	loadVec(t, "execution.json", &steps)
	var now float64
	ex := &ContactExecutor{Pool: app, Now: func() time.Time {
		return time.Unix(0, 0).Add(time.Duration(now * float64(time.Second)))
	}}
	for _, st := range steps {
		if st.Name == "running_blocks" { // the recorder seeds a crashed claimant here
			contract, _ := canonjson.Decode(steps[0].Args.Contract)
			hash, _ := Fingerprint(contract)
			if _, err := owner.Exec(context.Background(), `INSERT INTO contact_executions
				(id, workspace_id, action_id, contract_hash, status, started_at, expires_at)
				VALUES ('crash-1', 'w1', 'a6', $1, 'running', 1010.0, 1100.0)`, hash); err != nil {
				t.Fatal(err)
			}
		}
		now = st.Now
		contract, err := canonjson.Decode(st.Args.Contract)
		if err != nil {
			t.Fatal(err)
		}
		if st.Name == "nan_contract" {
			contract = map[string]any{"x": math.NaN()}
		}
		op := func(context.Context) (map[string]any, error) { return st.Payload, nil }
		switch st.Op {
		case "raise":
			op = func(context.Context) (map[string]any, error) { return nil, errors.New("boom") }
		case "slow":
			op = func(ctx context.Context) (map[string]any, error) {
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
				}
				return map[string]any{"ok": true}, nil
			}
		case "notdict":
			op = func(context.Context) (map[string]any, error) { return nil, nil }
		}
		got, err := ex.ExecuteOnce(context.Background(), st.Args.WorkspaceID, st.Args.ActionID, contract, op,
			time.Duration(st.Args.TimeoutSeconds*float64(time.Second)))
		if st.Raises != "" {
			if err == nil {
				t.Errorf("%s: want error (%s)", st.Name, st.Raises)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", st.Name, err)
		}
		sameJSON(t, st.Name+" result", got, st.Result)

		rows, err := owner.Query(context.Background(), `SELECT workspace_id, action_id, contract_hash, status, started_at, expires_at, result::text
			FROM contact_executions WHERE workspace_id IN ('w1','w2') ORDER BY workspace_id, action_id`)
		if err != nil {
			t.Fatal(err)
		}
		var have []map[string]any
		for rows.Next() {
			var ws, act, hash, status string
			var started, expires float64
			var res *string
			if err := rows.Scan(&ws, &act, &hash, &status, &started, &expires, &res); err != nil {
				t.Fatal(err)
			}
			var parsed any
			if res != nil {
				_ = json.Unmarshal([]byte(*res), &parsed)
			}
			have = append(have, map[string]any{"workspace_id": ws, "action_id": act, "contract_hash": hash, "status": status,
				"started_at": started, "expires_at": expires, "result": parsed})
		}
		rows.Close()
		var want []map[string]any
		for _, r := range st.Rows {
			var parsed any
			if r.Result != nil {
				_ = json.Unmarshal([]byte(*r.Result), &parsed)
			}
			want = append(want, map[string]any{"workspace_id": r.WorkspaceID, "action_id": r.ActionID, "contract_hash": r.ContractHash,
				"status": r.Status, "started_at": r.StartedAt, "expires_at": r.ExpiresAt, "result": parsed})
		}
		sameJSON(t, st.Name+" rows", have, want)
	}
}

// Many callers racing for one action key: exactly one claims and runs the operation.
func TestContactExecutionSingleFlight(t *testing.T) {
	app, _ := execPools(t)
	ex := &ContactExecutor{Pool: app}
	var mu sync.Mutex
	runs := 0
	op := func(context.Context) (map[string]any, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return map[string]any{"ok": true, "n": 1}, nil
	}
	var wg sync.WaitGroup
	results := make([]map[string]any, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := ex.ExecuteOnce(context.Background(), "wp-race", "act", map[string]any{"c": "x"}, op, 5*time.Second)
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}()
	}
	wg.Wait()
	if runs != 1 {
		t.Fatalf("operation ran %d times", runs)
	}
	completed := 0
	for _, r := range results {
		switch r["execution_status"] {
		case ExecCompleted:
			completed++
		case ExecRunning:
			if r["error"] != "contact_action_in_progress" {
				t.Errorf("blocked = %v", r)
			}
		default:
			t.Errorf("unexpected = %v", r)
		}
	}
	if completed != 1 {
		t.Fatalf("%d completed", completed)
	}
}

func TestContactExecutionCancelMarksUncertain(t *testing.T) {
	app, owner := execPools(t)
	ex := &ContactExecutor{Pool: app}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := ex.ExecuteOnce(ctx, "wp-race", "cancelled", map[string]any{"c": 1}, func(context.Context) (map[string]any, error) {
		cancel()
		time.Sleep(20 * time.Millisecond)
		return map[string]any{"ok": true}, nil
	}, 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	var status string
	if err := owner.QueryRow(context.Background(), `SELECT status FROM contact_executions WHERE workspace_id='wp-race' AND action_id='cancelled'`).Scan(&status); err != nil || status != ExecUncertain {
		t.Fatalf("status = %q, %v", status, err)
	}
}
