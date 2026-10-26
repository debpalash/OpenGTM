//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type wEnv struct {
	*authEnv
	ws    string
	uid   int32
	ctx   Ctx
	calls atomic.Int32
	cfg   Config
}

// newWEnv builds a workspace with one editor member and a Server whose only
// write tool is a fake create_lead that inserts a workbook row named by args["wid"].
func newWEnv(t *testing.T, role string) *wEnv {
	t.Helper()
	a := newAuthEnv(t)
	e := &wEnv{authEnv: a}
	e.uid = a.user(t, "u")
	e.ws = a.workspace(t, "w", &e.uid, "")
	if role != "" {
		a.member(t, e.ws, e.uid, role)
	}
	e.ctx = Ctx{WorkspaceID: e.ws, UserID: &e.uid, Capabilities: AllCaps}
	e.cfg = Config{WriteEnabled: true}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = a.pool.Exec(c, `DELETE FROM mcp_audit_log WHERE workspace_id=$1`, e.ws)
		_, _ = a.pool.Exec(c, `DELETE FROM trigger_cap_reservations WHERE workspace_id=$1`, e.ws)
		_, _ = a.pool.Exec(c, `DELETE FROM workbooks WHERE workspace_id=$1`, e.ws)
	})
	return e
}

func (e *wEnv) server(tools ...WriteTool) *Server {
	d := e.d
	d.Cfg = e.cfg
	return NewServer(d, NewRegistry(nil, tools))
}

// insertTool inserts a workbook per call (id = args["wid"]) and returns result.
func (e *wEnv) insertTool(name, capability string, roles []string, result string) WriteTool {
	return WriteTool{Name: name, Capability: capability, Roles: roles,
		Apply: func(ctx context.Context, tx pgx.Tx, c Ctx, args Args) (string, error) {
			e.calls.Add(1)
			wid, _, _ := args.String("wid")
			if _, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1,'n',$2)`, wid, c.WorkspaceID); err != nil {
				return "", err
			}
			return result, nil
		}}
}

func (e *wEnv) leadTool(result string) WriteTool {
	return e.insertTool("create_lead", CapLeadsWrite, WriteRoles, result)
}

func (e *wEnv) call(s *Server, name string, args Args) string {
	return s.callTool(context.Background(), e.ctx, name, args)
}

func (e *wEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *wEnv) workbooks(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM workbooks WHERE workspace_id=$1`, e.ws)
}

func (e *wEnv) audit(t *testing.T) []struct{ Tool, Status, Err, Key string } {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT tool_name, result_status, coalesce(error,''), coalesce(idempotency_key,'') FROM mcp_audit_log WHERE workspace_id=$1 ORDER BY created_at, id`, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct{ Tool, Status, Err, Key string }
	for rows.Next() {
		var r struct{ Tool, Status, Err, Key string }
		if err := rows.Scan(&r.Tool, &r.Status, &r.Err, &r.Key); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *wEnv) wantAudit(t *testing.T, n int, status, errPart string) {
	t.Helper()
	a := e.audit(t)
	if len(a) != n {
		t.Fatalf("audit rows = %d, want %d: %+v", len(a), n, a)
	}
	last := a[len(a)-1]
	if last.Status != status || !strings.Contains(last.Err, errPart) {
		t.Fatalf("last audit = %+v, want %s/%q", last, status, errPart)
	}
}

func (e *wEnv) held(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM trigger_cap_reservations WHERE workspace_id=$1 AND state='held'`, e.ws)
}

func TestWriteDisabledAuditedDenied(t *testing.T) {
	e := newWEnv(t, "editor")
	e.cfg.WriteEnabled = false
	s := e.server(e.leadTool(`{"status":"created"}`))
	if got := e.call(s, "create_lead", Args{"wid": "a"}); got != `{"error": "mcp writes disabled"}` {
		t.Fatal(got)
	}
	e.wantAudit(t, 1, "denied", "mcp writes disabled")
	if e.calls.Load() != 0 {
		t.Fatal("applied")
	}
}

func TestCapabilityDeniedAuditedForWritesOnly(t *testing.T) {
	e := newWEnv(t, "editor")
	e.ctx.Capabilities = map[string]bool{CapLeadsRead: true}
	s := e.server(e.leadTool(`{}`))
	e.call(s, "create_lead", Args{})
	e.call(s, "find_leads", Args{})
	e.call(s, "nope", Args{})
	e.wantAudit(t, 1, "denied", "capability not granted")
}

func TestGateDeniedAudited(t *testing.T) {
	e := newWEnv(t, "owner")
	for _, reason := range []string{"legacy_outreach_disabled", "automations disabled"} {
		tool := e.leadTool(`{}`)
		tool.Gate = func(Config) string { return reason }
		s := e.server(tool)
		if got := e.call(s, "create_lead", Args{"wid": "g"}); got != `{"error": "`+reason+`"}` {
			t.Fatal(got)
		}
		e.wantAudit(t, len(e.audit(t)), "denied", reason)
	}
	if e.calls.Load() != 0 || e.held(t) != 0 {
		t.Fatal("gate must precede any reservation")
	}
}

func TestRoleDowngradeDeniedAndAudited(t *testing.T) {
	e := newWEnv(t, "viewer")
	s := e.server(e.leadTool(`{"status":"created"}`))
	if got := e.call(s, "create_lead", Args{"wid": "r"}); got != `{"error": "insufficient workspace role"}` {
		t.Fatal(got)
	}
	e.wantAudit(t, 1, "denied", "insufficient workspace role")
	if e.workbooks(t) != 0 || e.held(t) != 0 {
		t.Fatal("side effects")
	}
}

func TestAdminOnlyToolsRejectEditor(t *testing.T) {
	e := newWEnv(t, "editor")
	s := e.server(
		e.insertTool("delete_lead", CapLeadsDelete, AdminRoles, `{"status":"deleted"}`),
		e.insertTool("send_email", CapOutreachSend, AdminRoles, `{"status":"queued"}`),
		e.insertTool("delete_workbook", CapWorkbooksDelete, AdminRoles, `{"status":"deleted"}`),
		e.insertTool("create_automation", CapAutomationsWrite, AdminRoles, `{"status":"created"}`),
	)
	for _, n := range []string{"delete_lead", "send_email", "delete_workbook", "create_automation"} {
		if got := e.call(s, n, Args{"wid": n}); got != `{"error": "insufficient workspace role"}` {
			t.Fatal(n, got)
		}
	}
	if e.calls.Load() != 0 || len(e.audit(t)) != 4 {
		t.Fatal(e.calls.Load(), len(e.audit(t)))
	}
}

func TestReplayDoesNotReapply(t *testing.T) {
	e := newWEnv(t, "editor")
	s := e.server(e.leadTool(`{"status":"created"}`))
	if got := e.call(s, "create_lead", Args{"wid": "a", "idempotency_key": "k1"}); got != `{"status":"created"}` {
		t.Fatal(got)
	}
	want := `{"status": "duplicate", "idempotency_key": "k1", "message": "already applied (idempotent replay)"}`
	if got := e.call(s, "create_lead", Args{"wid": "b", "idempotency_key": "k1"}); got != want {
		t.Fatal(got)
	}
	if e.calls.Load() != 1 || e.workbooks(t) != 1 {
		t.Fatal("reapplied")
	}
	e.wantAudit(t, 2, "ok", "idempotent_replay")
	if a := e.audit(t); a[0].Status != "ok" || a[0].Key != "k1" {
		t.Fatalf("%+v", a)
	}
}

func TestFailedWriteRollsBackAndKeyRetryable(t *testing.T) {
	e := newWEnv(t, "editor")
	var fail atomic.Bool
	fail.Store(true)
	tool := e.leadTool(`{"status":"created"}`)
	inner := tool.Apply
	tool.Apply = func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
		if _, err := inner(ctx, tx, c, a); err != nil {
			return "", err
		}
		if fail.Load() {
			return "", &ArgError{Field: "company", Want: "a string"}
		}
		return `{"status":"created"}`, nil
	}
	s := e.server(tool)
	if got := e.call(s, "create_lead", Args{"wid": "a", "idempotency_key": "k"}); got != `{"error": "company must be a string"}` {
		t.Fatal(got)
	}
	if e.workbooks(t) != 0 || e.held(t) != 0 {
		t.Fatal("mutation or reservation survived")
	}
	e.wantAudit(t, 1, "error", "company must be a string")
	fail.Store(false)
	if got := e.call(s, "create_lead", Args{"wid": "a", "idempotency_key": "k"}); got != `{"status":"created"}` {
		t.Fatal(got)
	}
	if e.workbooks(t) != 1 {
		t.Fatal("retry did not apply")
	}
	if a := e.audit(t); a[len(a)-1].Status != "ok" || a[len(a)-1].Key != "k" {
		t.Fatalf("%+v", a)
	}
}

func TestDatabaseErrorTextMasked(t *testing.T) {
	e := newWEnv(t, "editor")
	s := e.server(e.leadTool(`{}`))
	e.call(s, "create_lead", Args{"wid": "dup"})
	got := e.call(s, "create_lead", Args{"wid": "dup"}) // primary key violation
	if got != `{"error": "internal error"}` {
		t.Fatal(got)
	}
}

func TestStructuredErrorResultFreesCapSlot(t *testing.T) {
	e := newWEnv(t, "editor")
	e.cfg.MaxWritesPerDay = 1
	s := e.server(e.leadTool(`{"error": "lead_id is required"}`))
	for i := 0; i < 3; i++ {
		if got := e.call(s, "create_lead", Args{"wid": "x"}); got != `{"error": "lead_id is required"}` {
			t.Fatal(i, got)
		}
	}
	if e.workbooks(t) != 0 || e.held(t) != 0 {
		t.Fatal("state left")
	}
	e.wantAudit(t, 3, "error", "lead_id is required")
	// slot free: a good write still fits under cap 1.
	s2 := e.server(e.leadTool(`{"status":"not_found"}`))
	if got := e.call(s2, "create_lead", Args{"wid": "y"}); got != `{"status":"not_found"}` {
		t.Fatal(got)
	}
	// not_found settles (Python parity): the next write is capped.
	if got := e.call(s2, "create_lead", Args{"wid": "z"}); got != `{"error": "mcp write cap exceeded"}` {
		t.Fatal(got)
	}
}

func TestCapExceededAudited(t *testing.T) {
	e := newWEnv(t, "editor")
	e.cfg.MaxWritesPerDay = 1
	s := e.server(e.leadTool(`{"status":"created"}`))
	e.call(s, "create_lead", Args{"wid": "a"})
	if got := e.call(s, "create_lead", Args{"wid": "b"}); got != `{"error": "mcp write cap exceeded"}` {
		t.Fatal(got)
	}
	e.wantAudit(t, 2, "capped", "daily MCP write cap exceeded")
	if e.workbooks(t) != 1 {
		t.Fatal("capped call applied")
	}
}

func TestConcurrentSameKeyAppliesOnce(t *testing.T) {
	e := newWEnv(t, "editor")
	s := e.server(e.leadTool(`{"status":"created"}`))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.call(s, "create_lead", Args{"wid": "same", "idempotency_key": "kk"})
		}()
	}
	wg.Wait()
	if e.calls.Load() != 1 || e.workbooks(t) != 1 {
		t.Fatalf("applied %d times", e.calls.Load())
	}
	if n := len(e.audit(t)); n != 6 {
		t.Fatalf("audit rows %d", n)
	}
}

func TestApplyPanicRollsBackAndAudits(t *testing.T) {
	e := newWEnv(t, "editor")
	tool := e.leadTool(``)
	tool.Apply = func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
		_, _ = tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ('p','n',$1)`, c.WorkspaceID)
		panic("secret token sk-123 boom")
	}
	s := e.server(tool)
	got := e.call(s, "create_lead", Args{"idempotency_key": "pk"})
	if got != `{"error": "internal error"}` {
		t.Fatal(got)
	}
	if e.workbooks(t) != 0 || e.held(t) != 0 {
		t.Fatal("state left")
	}
	e.wantAudit(t, 1, "error", "internal error")
	if strings.Contains(e.audit(t)[0].Err, "sk-123") {
		t.Fatal("panic text leaked into audit")
	}
}

func TestOneAuditRowPerAttempt(t *testing.T) {
	e := newWEnv(t, "editor")
	e.cfg.MaxWritesPerDay = 2
	s := e.server(e.leadTool(`{"status":"created"}`))
	e.call(s, "create_lead", Args{"wid": "1", "idempotency_key": "a"}) // ok
	e.call(s, "create_lead", Args{"wid": "1", "idempotency_key": "a"}) // replay
	e.call(s, "create_lead", Args{"wid": "2"})                         // ok
	e.call(s, "create_lead", Args{"wid": "3"})                         // capped
	e.call(s, "create_lead", Args{"idempotency_key": 5})               // bad key
	if n := len(e.audit(t)); n != 5 {
		t.Fatalf("rows %d: %+v", n, e.audit(t))
	}
}

func TestIdempotencyKeyValidation(t *testing.T) {
	e := newWEnv(t, "editor")
	s := e.server(e.leadTool(`{"status":"created"}`))
	want := `{"error": "idempotency_key must be a string up to 128 characters"}`
	for _, k := range []any{float64(5), true, []any{}, strings.Repeat("k", 129)} {
		if got := e.call(s, "create_lead", Args{"wid": "v", "idempotency_key": k}); got != want {
			t.Fatalf("%v: %s", k, got)
		}
	}
	if e.calls.Load() != 0 {
		t.Fatal("applied")
	}
	// 128 chars, empty and null are accepted (empty/null generate a key).
	for _, k := range []any{strings.Repeat("k", 128), "", nil} {
		id := "v" + time.Now().Format("150405.000000000")
		if got := e.call(s, "create_lead", Args{"wid": id, "idempotency_key": k}); got != `{"status":"created"}` {
			t.Fatalf("%v: %s", k, got)
		}
	}
}

func TestWorkspaceInArgumentsIgnored(t *testing.T) {
	e := newWEnv(t, "editor")
	s := e.server(e.leadTool(`{"status":"created"}`))
	e.call(s, "create_lead", Args{"wid": "ws1", "workspace_id": "other-ws", "workspace": "x"})
	if e.workbooks(t) != 1 {
		t.Fatal("not applied in token workspace")
	}
	if e.count(t, `SELECT count(*) FROM mcp_audit_log WHERE workspace_id='other-ws'`) != 0 {
		t.Fatal("audited elsewhere")
	}
}

func TestCrashBetweenReserveAndApplyLeavesNoHeldRow(t *testing.T) {
	e := newWEnv(t, "editor")
	tool := e.leadTool(``)
	tool.Apply = func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
		var ok bool
		err := tx.QueryRow(ctx, `SELECT pg_terminate_backend(pg_backend_pid())`).Scan(&ok)
		return `{"status":"created"}`, err
	}
	s := e.server(tool)
	got := e.call(s, "create_lead", Args{"idempotency_key": "crash"})
	if !strings.HasPrefix(got, `{"error": `) {
		t.Fatal(got)
	}
	if e.held(t) != 0 || e.count(t, `SELECT count(*) FROM trigger_cap_reservations WHERE workspace_id=$1`, e.ws) != 0 {
		t.Fatal("reservation survived the crash")
	}
	e.wantAudit(t, 1, "error", "")
	s2 := e.server(e.leadTool(`{"status":"created"}`))
	if got := e.call(s2, "create_lead", Args{"wid": "after", "idempotency_key": "crash"}); got != `{"status":"created"}` {
		t.Fatal(got)
	}
}

func TestTimeoutDoesNotCommitAndReleases(t *testing.T) {
	e := newWEnv(t, "editor")
	tool := e.leadTool(``)
	done := make(chan struct{})
	tool.Apply = func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
		defer close(done)
		_, _ = tx.Exec(context.Background(), `INSERT INTO workbooks (id, name, workspace_id) VALUES ('late','n',$1)`, c.WorkspaceID)
		time.Sleep(300 * time.Millisecond) // ignores ctx
		return `{"status":"created"}`, nil
	}
	s := e.server(tool)
	s.Timeout = 100 * time.Millisecond
	got := e.call(s, "create_lead", Args{"idempotency_key": "slow"})
	if !strings.Contains(got, "timed out") {
		t.Fatal(got)
	}
	<-done
	time.Sleep(300 * time.Millisecond) // let the abandoned goroutine finish and audit
	if e.workbooks(t) != 0 || e.held(t) != 0 {
		t.Fatal("write committed after the client saw a timeout")
	}
	e.wantAudit(t, 1, "error", "")
}

func TestInFlightCapPerWorkspace(t *testing.T) {
	e := newWEnv(t, "editor")
	block := make(chan struct{})
	started := make(chan struct{}, 4)
	tool := e.leadTool(``)
	tool.Apply = func(ctx context.Context, tx pgx.Tx, c Ctx, a Args) (string, error) {
		started <- struct{}{}
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return `{"status":"created"}`, nil
	}
	s := e.server(tool)
	s.MaxInFlightWrites = 1
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.call(s, "create_lead", Args{}) }()
	<-started
	if got := e.call(s, "create_lead", Args{}); got != `{"error": "too many concurrent writes"}` {
		t.Fatal(got)
	}
	close(block)
	wg.Wait()
	if got := e.call(s, "create_lead", Args{}); got != `{"status":"created"}` {
		t.Fatalf("slot not released: %s", got)
	}
}

type bootPort struct {
	noAutomation
	mu    sync.Mutex
	calls [][2]string
	err   error
}

func (b *bootPort) BootstrapSchedule(_ context.Context, ws, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, [2]string{ws, id})
	return b.err
}

func TestBootstrapScheduleIsPostCommitBestEffort(t *testing.T) {
	e := newWEnv(t, "owner")
	bp := &bootPort{err: errors.New("boom")}
	e.d.Automation = bp
	okRes, _ := json.Marshal(map[string]any{"status": "created", "automation_id": "a1",
		"automation": map[string]any{"id": "a1", "trigger_type": "on_schedule", "enabled": true}})
	s := e.server(e.insertTool("create_automation", CapAutomationsWrite, AdminRoles, string(okRes)))
	if got := e.call(s, "create_automation", Args{"wid": "a1"}); got != string(okRes) {
		t.Fatal(got) // hook error must not fail the call
	}
	if len(bp.calls) != 1 || bp.calls[0] != [2]string{e.ws, "a1"} {
		t.Fatalf("%v", bp.calls)
	}
	e.wantAudit(t, 1, "ok", "")
	// a rolled back create never bootstraps; a non-schedule rule never does.
	bp.calls = nil
	s = e.server(e.insertTool("create_automation", CapAutomationsWrite, AdminRoles, `{"error": "bad"}`))
	e.call(s, "create_automation", Args{"wid": "a2"})
	other, _ := json.Marshal(map[string]any{"status": "created", "automation_id": "a3",
		"automation": map[string]any{"id": "a3", "trigger_type": "on_event", "enabled": true}})
	s = e.server(e.insertTool("create_automation", CapAutomationsWrite, AdminRoles, string(other)))
	e.call(s, "create_automation", Args{"wid": "a3"})
	if len(bp.calls) != 0 {
		t.Fatalf("%v", bp.calls)
	}
}
