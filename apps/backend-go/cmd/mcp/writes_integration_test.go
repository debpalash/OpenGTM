//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// The binary serves enroll_leads, send_email and create_automation through the
// production adapters (outreach store, send queue, autosvc): one stdio session
// with every write gate open.
func TestStdioOutreachAndAutomationWritesBinary(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	raw := "ycp_t17_all_" + s.ws
	h := sha256.Sum256([]byte(raw))
	if _, err := s.pool.Exec(ctx, `INSERT INTO mcp_tokens (id, token_hash, prefix, name, user_id, workspace_id, capabilities)
		SELECT $1, $2, 'ycp_x', '', owner_id, id, $3::json FROM workspaces WHERE id=$4`,
		"t17-all-"+s.ws, hex.EncodeToString(h[:]),
		`["leads:read","sequences:enroll","outreach:send","automations:write"]`, s.ws); err != nil {
		t.Fatal(err)
	}
	seq := "seq-" + s.ws
	inWS := func(q string, args ...any) {
		t.Helper()
		if err := tenancy.InWorkspace(ctx, s.pool, s.ws, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	inWS(`INSERT INTO outreach_sequences (id, workspace_id, name, description, steps, status, created_at, updated_at)
		VALUES ($1,$2,'s','','[]'::json,'active',now(),now())`, seq, s.ws)
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(ctx, s.pool, s.ws, func(tx pgx.Tx) error {
			for _, q := range []string{`DELETE FROM jobs`, `DELETE FROM outreach_enrollments`, `DELETE FROM outreach_sequences`, `DELETE FROM triggers`} {
				_, _ = tx.Exec(ctx, q+` WHERE workspace_id=$1`, s.ws)
			}
			return nil
		})
	})

	lead := strconv.FormatInt(s.leads[0], 10)
	calls := []struct{ args, want string }{
		{`"enroll_leads", "arguments": {"sequence_id": "` + seq + `", "lead_ids": [` + lead + `]}`,
			`{"enrolled": 1, "skipped": [], "total_lead_ids": 1}`},
		{`"send_email", "arguments": {"lead_id": ` + lead + `, "subject": "Hi", "body_html": "<p>x</p>"}`,
			`{"status": "success", "summary": "send enqueued"}`},
		{`"create_automation", "arguments": {"name": "leak", "trigger_type": "on_signal", "actions": [{"type": "webhook", "config": {"url": "https://h.example.com/x", "header_secret_ref": "SMTP_PASSWORD"}}]}`,
			`{"error": "header_secret_ref is not an available webhook secret", "status_code": 422}`},
		{`"create_automation", "arguments": {"name": "hook", "trigger_type": "on_signal", "actions": [{"type": "webhook", "config": {"url": "https://h.example.com/x"}}]}`,
			`{"status": "created", "automation_id": "`},
	}
	var stdin strings.Builder
	for i, c := range calls {
		stdin.WriteString(frame(`{"jsonrpc": "2.0", "id": ` + strconv.Itoa(i+1) + `, "method": "tools/call", "params": {"name": ` + c.args + `}}`))
	}
	out, errs, code := runBinary(t, baseEnv("DATABASE_URL="+appURL(t), "OPENGTM_MCP_TOKEN="+raw, "MCP_WRITE_ENABLED=true",
		"AUTOMATIONS_ENABLED=true", "AUTOMATIONS_ALLOW_LEGACY_OUTREACH=true"), stdin.String())
	frames := parseFrames(t, out)
	if code != 0 || len(frames) != len(calls) {
		t.Fatalf("code=%d frames=%d stderr=%s", code, len(frames), errs)
	}
	for i, f := range frames {
		var r struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(f), &r); err != nil || len(r.Result.Content) != 1 {
			t.Fatalf("frame %d: %q", i, f)
		}
		if got := r.Result.Content[0].Text; !strings.HasPrefix(got, calls[i].want) {
			t.Errorf("call %d:\n got %s\nwant %s", i+1, got, calls[i].want)
		}
	}
	var jobs, rules int
	var executor string
	if err := tenancy.InWorkspace(ctx, s.pool, s.ws, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(min(executor), '') FROM jobs WHERE workspace_id=$1 AND type='send'`, s.ws).Scan(&jobs, &executor); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM triggers WHERE workspace_id=$1`, s.ws).Scan(&rules)
	}); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || executor != "go" || rules != 1 {
		t.Fatalf("send jobs %d (%s), rules %d", jobs, executor, rules)
	}
}
