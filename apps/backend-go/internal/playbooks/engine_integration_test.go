//go:build integration

package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

type call struct {
	ws  string
	req ResearchRequest
}

// fakeRes is a scripted Researcher; fn decides the answer per call.
type fakeRes struct {
	mu    sync.Mutex
	calls []call
	fn    func(n int, c call) (ResearchResult, error)
}

func (f *fakeRes) Research(ctx context.Context, ws string, req ResearchRequest) (ResearchResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{ws, req})
	n := len(f.calls)
	f.mu.Unlock()
	if f.fn == nil {
		return ResearchResult{Success: true, Value: "v:" + req.PromptTemplate}, nil
	}
	return f.fn(n, call{ws, req})
}

func (e *env) engineDeps(r Researcher) Deps { return Deps{Pool: e.pool, Researcher: r} }

func (e *env) member(t *testing.T, ws, aud string, lead int) {
	t.Helper()
	e.sql(t, ws, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot) VALUES ($1,$2,$3,$4::json)`,
		ws, aud, lead, fmt.Sprintf(`{"lead": %d, "name": "L%d"}`, lead, lead))
}

func (e *env) members(t *testing.T, leads ...int) {
	for _, l := range leads {
		e.member(t, e.ws, e.aud, l)
	}
}

// engineRun creates a playbook with the given steps JSON snapshot and a run over e.aud.
func (e *env) engineRun(t *testing.T, stepsJSON string, maxMembers int) (Playbook, Run) {
	t.Helper()
	pb := e.mkPlaybook(t, e.ws, fmt.Sprintf("eng-%d", time.Now().UnixNano()))
	run := e.mkRun(t, e.ws, pb, e.aud)
	e.sql(t, e.ws, `UPDATE playbook_runs SET steps_snapshot=$3::json, max_members=$4 WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID, stepsJSON, maxMembers)
	return pb, run
}

const oneStep = `[{"key":"s1","name":"S1","prompt_template":"step one","output_format":"text"}]`

const threeSteps = `[{"key":"s1","name":"S1","prompt_template":"p1","output_format":"text"},
{"key":"s2","name":"S2","prompt_template":"p2","output_format":"json"},
{"key":"s3","name":"S3","prompt_template":"p3","output_format":"text"}]`

type resRow struct {
	lead     int
	status   string
	value    string
	meta     string
	err      *string
	attempts int
}

func (e *env) results(t *testing.T, ws, run string) []resRow {
	t.Helper()
	var out []resRow
	e.in(t, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT lead_id,status,value,result_metadata::text,error,attempts FROM playbook_results WHERE workspace_id=$1 AND run_id=$2 ORDER BY lead_id`, ws, run)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r resRow
			if err := rows.Scan(&r.lead, &r.status, &r.value, &r.meta, &r.err, &r.attempts); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out
}

type runRow struct {
	status                       string
	attempted, succeeded, failed int
	err                          *string
	started, finished            *time.Time
}

func (e *env) runState(t *testing.T, ws, id string) runRow {
	t.Helper()
	var r runRow
	e.in(t, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status,attempted,succeeded,failed,error,started_at,finished_at FROM playbook_runs WHERE workspace_id=$1 AND id=$2`, ws, id).
			Scan(&r.status, &r.attempted, &r.succeeded, &r.failed, &r.err, &r.started, &r.finished)
	})
	return r
}

func (e *env) run(t *testing.T, r Researcher, run Run) error {
	t.Helper()
	return HandleRun(context.Background(), e.engineDeps(r), e.ws, run.ID)
}

func leadOf(c call) int {
	n, _ := strconv.Atoi(fmt.Sprint(c.req.Context["lead"]))
	return n
}

func (e *env) leadsCalled(f *fakeRes) []int {
	var out []int
	for _, c := range f.calls {
		out = append(out, leadOf(c))
	}
	return out
}

func eqInts(a, b []int) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func TestRunProcessesMembersInLeadOrder(t *testing.T) {
	e := setup(t)
	e.members(t, 30, 10, 20)
	_, run := e.engineRun(t, oneStep, 100)
	f := &fakeRes{}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	if got := e.leadsCalled(f); !eqInts(got, []int{10, 20, 30}) {
		t.Fatalf("order %v", got)
	}
	for _, c := range f.calls {
		if c.ws != e.ws {
			t.Fatalf("workspace %q", c.ws)
		}
	}
	rs := e.results(t, e.ws, run.ID)
	if len(rs) != 3 || rs[0].lead != 10 || rs[0].status != "success" || rs[0].value != "v:step one" || rs[0].attempts != 1 || rs[0].err != nil {
		t.Fatalf("results %+v", rs)
	}
	var meta struct {
		Steps []struct {
			Key, Name string
			Success   bool
			Value     string
			Metadata  map[string]any
			Error     *string
		}
		CompletedSteps int `json:"completed_steps"`
		TotalSteps     int `json:"total_steps"`
	}
	if err := json.Unmarshal([]byte(rs[0].meta), &meta); err != nil || meta.CompletedSteps != 1 || meta.TotalSteps != 1 ||
		len(meta.Steps) != 1 || meta.Steps[0].Key != "s1" || meta.Steps[0].Name != "S1" || !meta.Steps[0].Success || meta.Steps[0].Error != nil {
		t.Fatalf("meta %s %v", rs[0].meta, err)
	}
	if !strings.HasPrefix(rs[0].meta, `{"steps":[{"key":"s1","name":"S1","success":true,"value":"v:step one","metadata":{},"error":null}],"completed_steps":1,"total_steps":1}`) {
		t.Fatalf("meta key order %s", rs[0].meta)
	}
	st := e.runState(t, e.ws, run.ID)
	if st.status != "completed" || st.attempted != 3 || st.succeeded != 3 || st.failed != 0 || st.started == nil || st.finished == nil || st.err != nil {
		t.Fatalf("run %+v", st)
	}
}

func TestRunMaxMembersLimit(t *testing.T) {
	e := setup(t)
	e.members(t, 3, 1, 2)
	_, run := e.engineRun(t, oneStep, 2)
	f := &fakeRes{}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	if got := e.leadsCalled(f); !eqInts(got, []int{1, 2}) {
		t.Fatalf("limited %v", got)
	}
}

func TestRunSkipsSuccessfulResultsOnRetry(t *testing.T) {
	e := setup(t)
	e.members(t, 10, 20, 30)
	_, run := e.engineRun(t, oneStep, 100)
	e.sql(t, e.ws, `INSERT INTO playbook_results (workspace_id,run_id,lead_id,status,value,result_metadata,attempts) VALUES ($1,$2,10,'success','old','{}',1)`, e.ws, run.ID)
	e.sql(t, e.ws, `INSERT INTO playbook_results (workspace_id,run_id,lead_id,status,value,result_metadata,error,attempts) VALUES ($1,$2,20,'failed','','{}','earlier',2)`, e.ws, run.ID)
	f := &fakeRes{}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	if got := e.leadsCalled(f); !eqInts(got, []int{20, 30}) {
		t.Fatalf("called %v", got)
	}
	rs := e.results(t, e.ws, run.ID)
	if rs[0].value != "old" || rs[0].attempts != 1 || rs[1].attempts != 3 || rs[1].err != nil || rs[1].status != "success" || rs[2].attempts != 1 {
		t.Fatalf("results %+v", rs)
	}
	if st := e.runState(t, e.ws, run.ID); st.succeeded != 3 || st.attempted != 3 || st.status != "completed" {
		t.Fatalf("run %+v", st)
	}
}

func TestRunStopsMemberAtFirstFailedStep(t *testing.T) {
	e := setup(t)
	e.members(t, 1, 2)
	_, run := e.engineRun(t, threeSteps, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		if c.req.PromptTemplate == "p2" {
			return ResearchResult{Success: false, Error: "step two broke", Value: "partial"}, nil
		}
		return ResearchResult{Success: true, Value: "ok-" + c.req.PromptTemplate, Metadata: map[string]any{"cost": 1}}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 4 { // s1, s2 per member; s3 never
		t.Fatalf("calls %d", len(f.calls))
	}
	rs := e.results(t, e.ws, run.ID)
	if rs[0].status != "failed" || rs[0].value != "partial" || rs[0].err == nil || *rs[0].err != "step two broke" {
		t.Fatalf("result %+v", rs[0])
	}
	var meta struct {
		Steps          []map[string]any
		CompletedSteps int `json:"completed_steps"`
		TotalSteps     int `json:"total_steps"`
	}
	if err := json.Unmarshal([]byte(rs[0].meta), &meta); err != nil || meta.CompletedSteps != 1 || meta.TotalSteps != 3 || len(meta.Steps) != 2 ||
		meta.Steps[1]["success"] != false || meta.Steps[1]["error"] != "step two broke" {
		t.Fatalf("meta %s", rs[0].meta)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "completed_with_errors" || st.failed != 2 {
		t.Fatalf("run %+v", st)
	}
}

func TestRunPassesPriorStepValuesInContext(t *testing.T) {
	e := setup(t)
	e.members(t, 7)
	_, run := e.engineRun(t, threeSteps, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		return ResearchResult{Success: true, Value: fmt.Sprintf("out%d", n)}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	c1, c2, c3 := f.calls[0].req.Context, f.calls[1].req.Context, f.calls[2].req.Context
	if _, has := c1["s1"]; has || c1["name"] != "L7" {
		t.Fatalf("ctx1 %v", c1)
	}
	if c2["s1"] != "out1" || c2["name"] != "L7" || len(c2) != 3 {
		t.Fatalf("ctx2 %v", c2)
	}
	if c3["s1"] != "out1" || c3["s2"] != "out2" {
		t.Fatalf("ctx3 %v", c3)
	}
	if f.calls[1].req.OutputFormat != "json" || f.calls[0].req.OutputFormat != "text" {
		t.Fatalf("formats %q %q", f.calls[0].req.OutputFormat, f.calls[1].req.OutputFormat)
	}
	if got := e.results(t, e.ws, run.ID)[0].value; got != "out3" {
		t.Fatalf("final value %q", got)
	}
}

func TestRunBudgetSplitAcrossSteps(t *testing.T) {
	e := setup(t)
	e.members(t, 1)
	pb, run := e.engineRun(t, threeSteps, 100)
	e.sql(t, e.ws, `UPDATE research_playbooks SET cell_budget_usd=0.3, max_steps=6 WHERE workspace_id=$1 AND id=$2`, e.ws, pb.ID)
	f := &fakeRes{}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	budget := 0.3
	want := budget / 3
	for _, c := range f.calls {
		if c.req.CellBudgetUSD != want || c.req.MaxSteps != 6 {
			t.Fatalf("budget %v steps %d", c.req.CellBudgetUSD, c.req.MaxSteps)
		}
	}
}

func TestRunDefaultStepWhenSnapshotEmpty(t *testing.T) {
	e := setup(t)
	e.members(t, 1)
	pb, run := e.engineRun(t, `[]`, 100)
	e.sql(t, e.ws, `UPDATE research_playbooks SET output_format='json', cell_budget_usd=0.2 WHERE workspace_id=$1 AND id=$2`, e.ws, pb.ID)
	f := &fakeRes{}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls %d", len(f.calls))
	}
	r := f.calls[0].req
	if r.PromptTemplate != "prompt template text" || r.OutputFormat != "json" || r.CellBudgetUSD != 0.2 {
		t.Fatalf("req %+v", r)
	}
	if !strings.Contains(e.results(t, e.ws, run.ID)[0].meta, `"key":"result","name":"Research"`) {
		t.Fatalf("meta %s", e.results(t, e.ws, run.ID)[0].meta)
	}
}

func TestRunCancelBeforeStepMarksCancelledResult(t *testing.T) {
	e := setup(t)
	e.members(t, 1, 2)
	_, run := e.engineRun(t, threeSteps, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		if n == 1 { // user cancels while step 1 runs
			e.sql(t, e.ws, `UPDATE playbook_runs SET status='cancelled', error='Cancelled by user', finished_at=(now() AT TIME ZONE 'UTC') WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID)
		}
		return ResearchResult{Success: true, Value: "x"}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls after cancel: %d", len(f.calls))
	}
	rs := e.results(t, e.ws, run.ID)
	if len(rs) != 1 || rs[0].status != "cancelled" || rs[0].err == nil || *rs[0].err != "Cancelled by user" {
		t.Fatalf("results %+v", rs)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "cancelled" || st.finished == nil {
		t.Fatalf("run %+v", st)
	}
}

func TestRunCancelBeforeMemberLeavesNoResult(t *testing.T) {
	e := setup(t)
	e.members(t, 1, 2)
	_, run := e.engineRun(t, oneStep, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		e.sql(t, e.ws, `UPDATE playbook_runs SET status='cancelled' WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID)
		return ResearchResult{Success: true, Value: "x"}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	rs := e.results(t, e.ws, run.ID)
	if len(f.calls) != 1 || len(rs) != 1 || rs[0].lead != 1 || rs[0].status != "success" {
		t.Fatalf("calls %d results %+v", len(f.calls), rs)
	}
}

func TestRunNeverOverwritesCancelledWithCompleted(t *testing.T) {
	e := setup(t)
	e.members(t, 1)
	_, run := e.engineRun(t, oneStep, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		e.sql(t, e.ws, `UPDATE playbook_runs SET status='cancelled', error='Cancelled by user' WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID)
		return ResearchResult{Success: true, Value: "x"}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "cancelled" || st.finished != nil && st.attempted != 0 {
		t.Fatalf("run %+v", st)
	}
}

func TestRunCountsFromLedger(t *testing.T) {
	e := setup(t)
	e.members(t, 1)
	_, run := e.engineRun(t, oneStep, 100)
	// ledger rows outside this attempt's members count too (SQL aggregate).
	e.sql(t, e.ws, `INSERT INTO playbook_results (workspace_id,run_id,lead_id,status,value,result_metadata,attempts) VALUES ($1,$2,50,'failed','','{}',1), ($1,$2,51,'success','','{}',1), ($1,$2,52,'cancelled','','{}',1)`, e.ws, run.ID)
	if err := e.run(t, &fakeRes{}, run); err != nil {
		t.Fatal(err)
	}
	st := e.runState(t, e.ws, run.ID)
	if st.attempted != 4 || st.succeeded != 2 || st.failed != 1 || st.status != "completed_with_errors" {
		t.Fatalf("run %+v", st)
	}
}

func TestRunStatusCompletedWithErrors(t *testing.T) {
	e := setup(t)
	e.members(t, 1, 2)
	_, run := e.engineRun(t, oneStep, 100)
	old := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	e.sql(t, e.ws, `UPDATE playbook_runs SET started_at=$3, error='stale' WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID, old)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		if n == 1 {
			return ResearchResult{Success: false, Error: strings.Repeat("e", 1500)}, nil
		}
		return ResearchResult{Success: true, Value: "fine"}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	st := e.runState(t, e.ws, run.ID)
	if st.status != "completed_with_errors" || st.failed != 1 || st.succeeded != 1 || st.err != nil || st.finished == nil || !st.started.Equal(old) {
		t.Fatalf("run %+v", st)
	}
	rs := e.results(t, e.ws, run.ID)
	if rs[0].err == nil || len(*rs[0].err) != 1000 || rs[0].status != "failed" {
		t.Fatalf("error not capped: %+v", rs[0])
	}
}

func TestRunResearcherErrorFailsOnlyThatMember(t *testing.T) {
	e := setup(t)
	e.members(t, 1, 2)
	_, run := e.engineRun(t, oneStep, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		if n == 1 {
			return ResearchResult{}, errors.New("pyworker: python worker auth rejected")
		}
		return ResearchResult{Success: true, Value: "fine"}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	rs := e.results(t, e.ws, run.ID)
	if rs[0].status != "failed" || rs[0].err == nil || !strings.Contains(*rs[0].err, "auth rejected") || rs[1].status != "success" {
		t.Fatalf("results %+v", rs)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "completed_with_errors" {
		t.Fatalf("run %+v", st)
	}
}

func TestRunUnavailableWorkerBeforeStartHandsBack(t *testing.T) {
	e := setup(t)
	e.members(t, 1, 2)
	_, run := e.engineRun(t, oneStep, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) { return ResearchResult{}, pyworker.ErrUnavailable }}
	err := e.run(t, f, run)
	if !errors.Is(err, pyworker.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls %d", len(f.calls))
	}
	st := e.runState(t, e.ws, run.ID)
	if st.status != "pending" || st.started != nil || st.finished != nil {
		t.Fatalf("run not handed back untouched: %+v", st)
	}
	for _, r := range e.results(t, e.ws, run.ID) {
		if r.status != "pending" || r.attempts != 0 {
			t.Fatalf("result %+v", r)
		}
	}
}

func TestRunUnavailableMidRunMarksPending(t *testing.T) {
	e := setup(t)
	e.members(t, 1, 2, 3)
	_, run := e.engineRun(t, oneStep, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		if n >= 2 {
			return ResearchResult{}, pyworker.ErrUnavailable
		}
		return ResearchResult{Success: true, Value: "x"}, nil
	}}
	err := e.run(t, f, run)
	if !errors.Is(err, pyworker.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	st := e.runState(t, e.ws, run.ID)
	if st.status != "pending" || st.finished != nil || st.started == nil {
		t.Fatalf("run %+v", st)
	}
	rs := e.results(t, e.ws, run.ID)
	if len(rs) != 2 || rs[0].status != "success" || rs[1].status != "pending" || rs[1].attempts != 0 {
		t.Fatalf("results %+v", rs)
	}
	// the retry resumes: member 1 is skipped, members 2 and 3 run.
	f2 := &fakeRes{}
	if err := e.run(t, f2, run); err != nil {
		t.Fatal(err)
	}
	if got := e.leadsCalled(f2); !eqInts(got, []int{2, 3}) {
		t.Fatalf("resume %v", got)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "completed" || st.succeeded != 3 {
		t.Fatalf("run %+v", st)
	}
}

func TestRunCapsOversizedValue(t *testing.T) {
	e := setup(t)
	e.members(t, 1)
	_, run := e.engineRun(t, oneStep, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		return ResearchResult{Success: true, Value: strings.Repeat("é", 40000)}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	v := e.results(t, e.ws, run.ID)[0].value
	if len(v) > 64*1024 || len(v) < 64*1024-1 {
		t.Fatalf("value bytes %d", len(v))
	}
}

func TestRunValueIsPythonStr(t *testing.T) {
	e := setup(t)
	e.members(t, 1, 2)
	_, run := e.engineRun(t, oneStep, 100)
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		if n == 1 {
			return ResearchResult{Success: true, Value: map[string]any{"a": true, "b": nil}}, nil
		}
		return ResearchResult{Success: true, Value: nil}, nil
	}}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	rs := e.results(t, e.ws, run.ID)
	if rs[0].value != `{'a': True, 'b': None}` || rs[1].value != "" {
		t.Fatalf("values %q %q", rs[0].value, rs[1].value)
	}
}

func TestRunOtherWorkspaceMembersNeverRead(t *testing.T) {
	e := setup(t)
	e.member(t, e.ws2, e.aud2, 1)
	e.member(t, e.ws2, e.aud2, 2)
	pb := e.mkPlaybook(t, e.ws, "xws")
	run := e.mkRun(t, e.ws, pb, e.aud)
	e.sql(t, e.ws, `UPDATE playbook_runs SET audience_id=$3 WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID, e.aud2) // foreign audience id
	f := &fakeRes{}
	if err := e.run(t, f, run); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || len(e.results(t, e.ws, run.ID)) != 0 {
		t.Fatalf("foreign members were read: %d calls", len(f.calls))
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "completed" || st.attempted != 0 {
		t.Fatalf("run %+v", st)
	}
	// and a run id of another workspace is simply absent.
	if err := HandleRun(context.Background(), e.engineDeps(f), e.ws2, run.ID); err != nil || len(f.calls) != 0 {
		t.Fatalf("foreign run: %v calls %d", err, len(f.calls))
	}
}

func TestRunNoopCases(t *testing.T) {
	e := setup(t)
	e.members(t, 1)
	f := &fakeRes{}
	if err := HandleRun(context.Background(), e.engineDeps(f), e.ws, "no-such-run"); err != nil {
		t.Fatalf("missing run: %v", err)
	}
	for _, st := range []string{"completed", "cancelled"} {
		_, run := e.engineRun(t, oneStep, 100)
		e.sql(t, e.ws, `UPDATE playbook_runs SET status=$3 WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID, st)
		if err := e.run(t, f, run); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		if got := e.runState(t, e.ws, run.ID); got.status != st {
			t.Fatalf("%s changed to %s", st, got.status)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("researcher called")
	}
	if err := HandleRun(context.Background(), e.engineDeps(f), "", "x"); err == nil {
		t.Fatal("empty workspace accepted")
	}
}

func TestRunFailedRunRerunsFromScratch(t *testing.T) {
	e := setup(t)
	e.members(t, 1)
	_, run := e.engineRun(t, oneStep, 100)
	e.sql(t, e.ws, `UPDATE playbook_runs SET status='failed', error='Final failure: x', finished_at=(now() AT TIME ZONE 'UTC') WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID)
	if err := e.run(t, &fakeRes{}, run); err != nil {
		t.Fatal(err)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "completed" || st.err != nil {
		t.Fatalf("run %+v", st)
	}
}

// --- reconcile ---

func payloadFor(ws, run string) []byte {
	b, _ := json.Marshal(map[string]any{"workspace_id": ws, "run_id": run})
	return b
}

func (e *env) seedLedger(t *testing.T, run string) {
	for i, s := range []string{"success", "failed", "running", "pending", "cancelled"} {
		e.sql(t, e.ws, `INSERT INTO playbook_results (workspace_id,run_id,lead_id,status,value,result_metadata,error,attempts) VALUES ($1,$2,$3,$4,'','{}','prev',1)`, e.ws, run, i+1, s)
	}
}

func TestReconcileVectorsMatchPython(t *testing.T) {
	type want struct {
		run      string
		runErr   string // "" = unchanged expectation not checked / nil
		finished bool
		statuses []string // lead 1..5 after reconcile
		errs     []string // result errors after ("prev" untouched)
		counts   [3]int   // attempted, succeeded, failed (only checked when countsCheck)
		counts_  bool
	}
	retryErr := "Queue retry scheduled: worker killed"
	finalErr := "Final failure: worker killed"
	vectors := []struct {
		name      string
		startRun  string
		willRetry bool
		msg       string
		want      want
	}{
		{"retry", "running", true, "worker killed", want{run: "pending", runErr: retryErr,
			statuses: []string{"success", "failed", "pending", "pending", "cancelled"}, errs: []string{"prev", "prev", retryErr, "prev", "prev"}}},
		{"final", "running", false, "worker killed", want{run: "failed", runErr: finalErr, finished: true,
			statuses: []string{"success", "failed", "failed", "failed", "cancelled"}, errs: []string{"prev", "prev", finalErr, finalErr, "prev"},
			counts: [3]int{5, 1, 3}, counts_: true}},
		{"retry from failed run", "failed", true, "worker killed", want{run: "pending", runErr: retryErr,
			statuses: []string{"success", "failed", "pending", "pending", "cancelled"}, errs: []string{"prev", "prev", retryErr, "prev", "prev"}}},
		{"completed untouched", "completed", false, "x", want{run: "completed", statuses: []string{"success", "failed", "running", "pending", "cancelled"}}},
		{"completed_with_errors untouched", "completed_with_errors", true, "x", want{run: "completed_with_errors", statuses: []string{"success", "failed", "running", "pending", "cancelled"}}},
		{"cancelled untouched", "cancelled", false, "x", want{run: "cancelled", statuses: []string{"success", "failed", "running", "pending", "cancelled"}}},
		{"empty message default", "running", false, "", want{run: "failed", runErr: "Final failure: playbook attempt failed", finished: true,
			statuses: []string{"success", "failed", "failed", "failed", "cancelled"}, errs: nil}},
		{"long message cut to 1000", "running", true, strings.Repeat("ü", 1200), want{run: "pending", runErr: "Queue retry scheduled: " + strings.Repeat("ü", 1000),
			statuses: []string{"success", "failed", "pending", "pending", "cancelled"}}},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			e := setup(t)
			_, run := e.engineRun(t, oneStep, 100)
			e.seedLedger(t, run.ID)
			e.sql(t, e.ws, `UPDATE playbook_runs SET status=$3, finished_at=NULL WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID, v.startRun)
			if err := ReconcileFailure(context.Background(), e.engineDeps(nil), 7, payloadFor(e.ws, run.ID), v.msg, v.willRetry); err != nil {
				t.Fatal(err)
			}
			st := e.runState(t, e.ws, run.ID)
			if st.status != v.want.run || (st.finished != nil) != v.want.finished {
				t.Fatalf("run %+v want %+v", st, v.want)
			}
			if v.want.runErr != "" && (st.err == nil || v.want.runErr != *st.err) {
				t.Fatalf("run error %v want %q", st.err, v.want.runErr)
			}
			rs := e.results(t, e.ws, run.ID)
			for i, r := range rs {
				if r.status != v.want.statuses[i] {
					t.Fatalf("lead %d status %s want %s", r.lead, r.status, v.want.statuses[i])
				}
				if v.want.errs != nil && (r.err == nil || *r.err != v.want.errs[i]) {
					t.Fatalf("lead %d error %v want %q", r.lead, r.err, v.want.errs[i])
				}
			}
			if v.want.counts_ && [3]int{st.attempted, st.succeeded, st.failed} != v.want.counts {
				t.Fatalf("counts %+v", st)
			}
		})
	}
}

func TestReconcileRejectsBadPayloadAndForeignRun(t *testing.T) {
	e := setup(t)
	_, run := e.engineRun(t, oneStep, 100)
	e.sql(t, e.ws, `UPDATE playbook_runs SET status='running' WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID)
	for _, p := range []string{`{}`, `{"workspace_id":"w"}`, `{"run_id":"r"}`, `{"workspace_id":"  ","run_id":"r"}`, `[]`, `nope`} {
		if err := ReconcileFailure(context.Background(), e.engineDeps(nil), 1, []byte(p), "x", false); err == nil ||
			err.Error() != "playbook failure payload requires workspace_id and run_id" {
			t.Fatalf("%s: %v", p, err)
		}
	}
	// a run of another workspace under this payload's workspace is not found: untouched.
	if err := ReconcileFailure(context.Background(), e.engineDeps(nil), 1, payloadFor(e.ws2, run.ID), "x", false); err != nil {
		t.Fatal(err)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "running" {
		t.Fatalf("foreign reconcile touched run: %+v", st)
	}
	// ids are stripped like Python's str().strip().
	if err := ReconcileFailure(context.Background(), e.engineDeps(nil), 1, payloadFor(" "+e.ws+" ", " "+run.ID+"\n"), "x", true); err != nil {
		t.Fatal(err)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "pending" {
		t.Fatalf("stripped ids: %+v", st)
	}
}

// A cancel that interrupts an in-flight step (the worker's cancel watcher cancels ctx) must
// not leave the member's result row `running`.
func TestRunCancelDuringStepLeavesNoRunningResult(t *testing.T) {
	e := setup(t)
	e.members(t, 1)
	_, run := e.engineRun(t, threeSteps, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeRes{fn: func(n int, c call) (ResearchResult, error) {
		e.sql(t, e.ws, `UPDATE playbook_runs SET status='cancelled', error='Cancelled by user', finished_at=(now() AT TIME ZONE 'UTC') WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID)
		cancel()
		return ResearchResult{}, ctx.Err()
	}}
	_ = HandleRun(ctx, e.engineDeps(f), e.ws, run.ID)
	rs := e.results(t, e.ws, run.ID)
	if len(rs) != 1 || rs[0].status != "cancelled" {
		t.Fatalf("results %+v", rs)
	}
}
