//go:build integration

package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/imapclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

const wsOut = "ws-p09t19"

type fakeMailer struct {
	mu    sync.Mutex
	n     int
	err   error
	block chan struct{}
}

func (f *fakeMailer) Send(ctx context.Context, _ outreachsend.MailConfig, _, _ string, _ []byte) (string, error) {
	f.mu.Lock()
	f.n++
	err := f.err
	f.mu.Unlock()
	return "<t19@example.com>", err
}
func (f *fakeMailer) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.n }

type fakeIMAP struct{ connects int }

func (f *fakeIMAP) Connect(context.Context) error                  { f.connects++; return nil }
func (f *fakeIMAP) Select(context.Context, string) (string, error) { return "42", nil }
func (f *fakeIMAP) SearchUnseen(context.Context, time.Time, int) ([]string, error) {
	return nil, nil
}
func (f *fakeIMAP) Fetch(context.Context, string) ([]byte, error) { return nil, nil }
func (f *fakeIMAP) MarkSeen(context.Context, string) error        { return nil }
func (f *fakeIMAP) Logout()                                       {}

type memSecrets struct{ ws map[string]string }

func (s *memSecrets) AppSetting(_ context.Context, k string) (string, error) {
	if k == "OUTREACH_FOOTER" {
		return "Contract Co, 1 Example Street", nil
	}
	return "", nil
}
func (s *memSecrets) WorkspaceSetting(_ context.Context, ws, k string) (string, bool, error) {
	v, ok := s.ws[ws+"/"+k]
	return v, ok, nil
}
func (s *memSecrets) SetWorkspaceSetting(_ context.Context, ws, k, v string) error {
	s.ws[ws+"/"+k] = v
	return nil
}

func outCfg() config.Config {
	cfg := e2eConfig()
	cfg.SecretKey = "a-strong-secret-key-of-32-bytes-or-more"
	cfg.Executor.Workspaces[wsOut] = true
	cfg.Flags.AutomationsEnabled = true
	cfg.Flags.Outreach = config.OutreachSettings{GoExecutorSend: true, GoExecutorInbound: true, InboundPollEnabled: true,
		PublicBaseURL: "https://app.example.com", UnsubTTLDays: 90, InboundPollInterval: 900, InboundMaxFetch: 100,
		InboundMaxConsecutiveFailures: 10, CircuitMinSends: 20, BouncePauseRate: 0.05, ComplaintPauseRate: 0.003, SoftBounceMax: 3}
	return cfg
}

func outResolver(t *testing.T, kv map[string]string) *secrets.Resolver {
	t.Helper()
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: "t19-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := secrets.NewResolver(e, &memSecrets{ws: map[string]string{}}, func(string) string { return "" })
	for k, v := range kv {
		if err := r.Set(context.Background(), wsOut, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

var outKV = map[string]string{"SMTP_HOST": "smtp.test.invalid", "SMTP_PORT": "587", "SMTP_EMAIL": "sender@contract.example.com",
	"SMTP_PASSWORD": "fake-password-not-real", "SMTP_USE_TLS": "1", "IMAP_HOST": "imap.test.invalid"}

type outEnv struct {
	pool *pgxpool.Pool
	ctx  context.Context
	cfg  config.Config
	rn   runner
	mail *fakeMailer
	imap *fakeIMAP
}

func newOutEnv(t *testing.T, cfg config.Config, noCaps bool) *outEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	e := &outEnv{pool: pool, ctx: ctx, cfg: cfg, mail: &fakeMailer{}, imap: &fakeIMAP{}}
	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM jobs`)
		_, _ = pool.Exec(ctx, `DELETE FROM outreach_inbound_schedules WHERE workspace_id=$1`, wsOut)
		_ = tenancy.InWorkspace(ctx, pool, wsOut, func(tx pgx.Tx) error {
			for _, q := range []string{"outreach_sends", "outreach_enrollments", "outreach_sequences", "outreach_suppressions", "credit_ledger_entries", "workspace_credits"} {
				_, _ = tx.Exec(ctx, "DELETE FROM "+q+" WHERE workspace_id=$1", wsOut)
			}
			return nil
		})
	}
	clean()
	t.Cleanup(clean)
	handlers := buildOutreach(cfg, pool, telemetry.NewLogger(io.Discard), outResolver(t, outKV), outreachOpts{
		mailer: e.mail, noCaps: noCaps,
		newIMAP: func(outreachcfg.IMAP) imapclient.Client { return e.imap },
	})
	e.rn = dispatch(handlers)
	return e
}

func (e *outEnv) seedSend(t *testing.T) string {
	t.Helper()
	idem := outreachsend.SequenceStepIdem(wsOut, "seq-1", 1, 0)
	if err := tenancy.InWorkspace(e.ctx, e.pool, wsOut, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `INSERT INTO outreach_sequences (id,workspace_id,name,description,steps,status,daily_limit,send_window_start,send_window_end,send_window_tz,consent_basis)
		  VALUES ('seq-1',$1,'Seq','',$2::json,'active',50,0,24,'UTC','legit')`, wsOut,
			`[{"step_number":0,"subject":"Hi {{name}}","body_html":"<p>Hello</p>","delay_hours":0}]`); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `INSERT INTO outreach_enrollments (id,workspace_id,sequence_id,lead_id,to_email_snapshot,current_step,status,next_send_at,sent_count,soft_bounce_count,error)
		  VALUES (1,$1,'seq-1',1,'ann@example.com',0,'pending',now() AT TIME ZONE 'UTC',0,0,'')`, wsOut)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return idem
}

func (e *outEnv) enqueue(t *testing.T, typ, payload, executor string, maxRetries int) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		VALUES ($1,$2::json,'pending',0,now() AT TIME ZONE 'UTC',0,$3,$4,$5)`, typ, payload, maxRetries, wsOut, executor); err != nil {
		t.Fatal(err)
	}
}

func sendPayload(idem string) string {
	return `{"workspace_id":"` + wsOut + `","sequence_id":"seq-1","enrollment_id":1,"lead_id":1,"step_number":0,"to_email":"ann@example.com","idempotency_key":"` + idem + `","variables":{"name":"Ann"},"fire_key":"` + idem + `"}`
}

// claimRun claims one go job and runs it through processJob (heartbeat, deadline, finalize).
func (e *outEnv) claimRun(t *testing.T) {
	t.Helper()
	c, err := claimNext(e.ctx, e.pool, e.cfg, "w")
	if err != nil || c == nil {
		t.Fatalf("claim %v err %v", c, err)
	}
	processJob(e.ctx, e.pool, e.cfg, telemetry.NewLogger(io.Discard), c, e.rn)
}

func (e *outEnv) job(t *testing.T, typ string) (status, executor string, retries int, nextRun *time.Time) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT status, executor, retry_count, next_run_at FROM jobs WHERE type=$1 ORDER BY id LIMIT 1`, typ).Scan(&status, &executor, &retries, &nextRun); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *outEnv) sendStatus(t *testing.T, idem string) string {
	t.Helper()
	var st string
	_ = tenancy.InWorkspace(e.ctx, e.pool, wsOut, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT status FROM outreach_sends WHERE workspace_id=$1 AND idempotency_key=$2`, wsOut, idem).Scan(&st)
	})
	return st
}

func TestWorkerHandlesSendJob(t *testing.T) {
	e := newOutEnv(t, outCfg(), false)
	idem := e.seedSend(t)
	e.enqueue(t, "send", sendPayload(idem), "go", 3)
	e.claimRun(t)
	if st, ex, _, _ := e.job(t, "send"); st != "completed" || ex != "go" {
		t.Fatalf("job %s/%s", st, ex)
	}
	if e.mail.count() != 1 || e.sendStatus(t, idem) != "sent" {
		t.Fatalf("mails %d send row %q", e.mail.count(), e.sendStatus(t, idem))
	}
}

func TestWorkerIgnoresPythonSendRows(t *testing.T) {
	e := newOutEnv(t, outCfg(), false)
	idem := e.seedSend(t)
	e.enqueue(t, "send", sendPayload(idem), "python", 3)
	e.enqueue(t, "outreach_inbound_poll", `{"workspace_id":"`+wsOut+`"}`, "python", 3)
	if c, err := claimNext(e.ctx, e.pool, e.cfg, "w"); err != nil || c != nil {
		t.Fatalf("claimed %v err %v", c, err)
	}
	if e.mail.count() != 0 {
		t.Fatal("python row was sent")
	}
}

func TestWorkerHandsBackPaidSendWithoutCaps(t *testing.T) {
	cfg := outCfg()
	cfg.Flags.Outreach.SendCostUSD = 0.02
	e := newOutEnv(t, cfg, true)
	idem := e.seedSend(t)
	e.enqueue(t, "send", sendPayload(idem), "go", 3)
	c, err := claimNext(e.ctx, e.pool, e.cfg, "w")
	if err != nil || c == nil {
		t.Fatalf("claim %v %v", c, err)
	}
	if err := e.rn(e.ctx, c); !errors.Is(err, workbook.ErrHandedBack) {
		t.Fatalf("err %v, want ErrHandedBack", err)
	}
	if st, ex, _, _ := e.job(t, "send"); st != "pending" || ex != "python" {
		t.Fatalf("job %s/%s", st, ex)
	}
	if e.mail.count() != 0 {
		t.Fatal("paid send went out unmetered")
	}
}

func TestWorkerSendRetryBackoff(t *testing.T) {
	e := newOutEnv(t, outCfg(), false)
	e.mail.err = errors.New("temporary relay trouble")
	idem := e.seedSend(t)
	e.enqueue(t, "send", sendPayload(idem), "go", 3)
	before := time.Now().UTC()
	e.claimRun(t)
	st, _, retries, next := e.job(t, "send")
	if st != "pending" || retries != 1 || next == nil || !next.After(before) {
		t.Fatalf("job %s retries %d next %v: transient send error must retry with the queue backoff", st, retries, next)
	}
}

func TestWorkerInboundRescheduleEnqueuesNext(t *testing.T) {
	e := newOutEnv(t, outCfg(), false)
	e.enqueue(t, "outreach_inbound_poll", `{"workspace_id":"`+wsOut+`"}`, "go", 3)
	e.claimRun(t)
	if e.imap.connects != 1 {
		t.Fatalf("connects %d", e.imap.connects)
	}
	var pending int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM jobs WHERE type='outreach_inbound_poll' AND status='pending' AND executor='go'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	var done int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM jobs WHERE type='outreach_inbound_poll' AND status='completed'`).Scan(&done)
	if pending != 1 || done != 1 {
		t.Fatalf("pending next polls %d completed %d", pending, done)
	}
}

func TestWorkerCancelMidSend(t *testing.T) {
	e := newOutEnv(t, outCfg(), false)
	idem := e.seedSend(t)
	e.enqueue(t, "send", sendPayload(idem), "go", 3)
	c, err := claimNext(e.ctx, e.pool, e.cfg, "w")
	if err != nil || c == nil {
		t.Fatalf("claim %v %v", c, err)
	}
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if err := e.rn(ctx, c); err == nil {
		t.Fatal("cancelled send returned no error")
	}
	if e.mail.count() != 0 {
		t.Fatal("send dialled after cancel")
	}
}
