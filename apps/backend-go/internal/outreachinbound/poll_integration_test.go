//go:build integration

package outreachinbound_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/imapclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const imapPassword = "imap-s3cret-DO-NOT-LEAK"

var th = outreachstore.Thresholds{MinSends: 20, BounceRate: 0.05, ComplaintRate: 0.003, SoftBounceMax: 3}

// ------------------------------------------------------------------ helpers

func newWS(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	ws := "p09t18-" + hex.EncodeToString(b)
	t.Cleanup(func() {
		ctx := context.Background()
		_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			for _, q := range []string{"outreach_inbound_messages", "outreach_suppressions", "outreach_sends", "outreach_enrollments", "outreach_sequences"} {
				if _, err := tx.Exec(ctx, "DELETE FROM "+q+" WHERE workspace_id=$1", ws); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, "DELETE FROM jobs WHERE workspace_id=$1", ws)
			return err
		})
		_, _ = pool.Exec(ctx, `DELETE FROM outreach_inbound_schedules WHERE workspace_id=$1`, ws)
	})
	return ws
}

func inTx(t *testing.T, pool *pgxpool.Pool, ws string, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error { fn(ctx, tx); return nil }); err != nil {
		t.Fatalf("tx: %v", err)
	}
}

type seeded struct {
	seq    string
	enr    int
	sendID int32
}

// seedSend creates a sequence, an enrollment and a sent send with this Message-ID.
func seedSend(t *testing.T, pool *pgxpool.Pool, ws, to, mid string, lead int) seeded {
	t.Helper()
	var s seeded
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		seq, err := outreachstore.CreateSequence(ctx, tx, ws, outreachstore.NewSeqInput("seq-"+mid))
		if err != nil {
			t.Fatal(err)
		}
		enr, _, err := outreachstore.Enroll(ctx, tx, ws, seq.ID, lead, to, "form", time.Now())
		if err != nil || enr == 0 {
			t.Fatalf("enroll: %d %v", enr, err)
		}
		e32 := int32(enr)
		idem := fmt.Sprintf("idem-%s-%d", mid, time.Now().UnixNano())
		ok, err := outreachstore.InsertInFlight(ctx, tx, outreachstore.SendIn{WorkspaceID: ws, SequenceID: seq.ID, EnrollmentID: &e32,
			StepNumber: 1, ToEmail: to, Subject: "hi", IdempotencyKey: idem})
		if err != nil || !ok {
			t.Fatalf("in flight: %v %v", ok, err)
		}
		now := time.Now()
		if err := outreachstore.FinalizeSend(ctx, tx, ws, idem, outreachstore.Finalize{Status: "sent", MessageID: mid, SentAt: &now}); err != nil {
			t.Fatal(err)
		}
		snd, err := outreachstore.GetSendByIdem(ctx, tx, ws, idem)
		if err != nil || snd == nil {
			t.Fatalf("get send: %v", err)
		}
		s = seeded{seq: seq.ID, enr: enr, sendID: snd.ID}
	})
	return s
}

type ledgerRow struct {
	UID, Kind, Recipient, Diagnostic, SourceMID string
	Matched                                     *int32
}

func ledger(t *testing.T, pool *pgxpool.Pool, ws string) []ledgerRow {
	t.Helper()
	var out []ledgerRow
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT imap_uid, COALESCE(kind,''), COALESCE(recipient,''), COALESCE(diagnostic,''), source_message_id, matched_send_id
			FROM outreach_inbound_messages WHERE workspace_id=$1 ORDER BY id`, ws)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r ledgerRow
			if err := rows.Scan(&r.UID, &r.Kind, &r.Recipient, &r.Diagnostic, &r.SourceMID, &r.Matched); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
	})
	return out
}

func process(t *testing.T, pool *pgxpool.Pool, ws, uid string, raw []byte) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		return outreachinbound.Process(ctx, tx, ws, "42", uid, raw, th)
	})
	if err != nil {
		t.Fatalf("process %s: %v", uid, err)
	}
}

func sendStatus(t *testing.T, pool *pgxpool.Pool, ws string, id int32) string {
	t.Helper()
	var st string
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT status FROM outreach_sends WHERE id=$1`, id).Scan(&st); err != nil {
			t.Fatal(err)
		}
	})
	return st
}

func seqCounts(t *testing.T, pool *pgxpool.Pool, ws, seq string) (bounce, complaint int) {
	t.Helper()
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(bounce_count,0), COALESCE(complaint_count,0) FROM outreach_sequences WHERE id=$1`, seq).Scan(&bounce, &complaint); err != nil {
			t.Fatal(err)
		}
	})
	return
}

func suppression(t *testing.T, pool *pgxpool.Pool, ws, email string) (reason string, locked, found bool) {
	t.Helper()
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		err := tx.QueryRow(ctx, `SELECT COALESCE(reason,''), COALESCE(locked,false) FROM outreach_suppressions WHERE workspace_id=$1 AND email=$2`, ws, email).Scan(&reason, &locked)
		found = err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
	})
	return
}

func enrStatus(t *testing.T, pool *pgxpool.Pool, ws string, id int) (status, errText string) {
	t.Helper()
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(status,''), COALESCE(error,'') FROM outreach_enrollments WHERE id=$1`, id).Scan(&status, &errText); err != nil {
			t.Fatal(err)
		}
	})
	return
}

func dsn(msgID, origMID, rcpt, status, diag string) []byte {
	return []byte(strings.ReplaceAll(fmt.Sprintf(`From: MAILER-DAEMON@mx.example.net
To: bounces@sender.example
Subject: Undelivered Mail Returned to Sender
Message-ID: %s
MIME-Version: 1.0
Content-Type: multipart/report; report-type=delivery-status; boundary="BND"

--BND
Content-Type: text/plain

Delivery failed.
--BND
Content-Type: message/delivery-status

Reporting-MTA: dns; mx.example.net

Final-Recipient: rfc822; %s
Action: failed
Status: %s
Diagnostic-Code: smtp; %s

--BND
Content-Type: message/rfc822

Message-ID: %s
From: a@sender.example
To: %s
Subject: hi

hello
--BND--
`, msgID, rcpt, status, diag, origMID, rcpt), "\n", "\r\n"))
}

// ------------------------------------------------------------ fake IMAP client

type fakeClient struct {
	mu           sync.Mutex
	uids         []string
	msgs         map[string][]byte
	uidValidity  string
	uidValidity2 string // returned by Select once reconnected
	connectErr   error
	selectErr    error
	fetchErrs    map[string][]error // consumed one per Fetch call
	markErr      error

	connects, logouts int
	seen, fetched     []string
	searchMax         int
	searchSince       time.Time
}

func (f *fakeClient) Connect(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	return f.connectErr
}
func (f *fakeClient) Select(context.Context, string) (string, error) {
	if f.selectErr != nil {
		return "", f.selectErr
	}
	if f.uidValidity2 != "" && f.connects >= 2 {
		return f.uidValidity2, nil
	}
	if f.uidValidity == "" {
		return "777", nil
	}
	return f.uidValidity, nil
}
func (f *fakeClient) SearchUnseen(_ context.Context, since time.Time, max int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchMax, f.searchSince = max, since
	return append([]string(nil), f.uids...), nil
}
func (f *fakeClient) Fetch(_ context.Context, uid string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, uid)
	if q := f.fetchErrs[uid]; len(q) > 0 {
		f.fetchErrs[uid] = q[1:]
		return nil, q[0]
	}
	return f.msgs[uid], nil
}
func (f *fakeClient) MarkSeen(_ context.Context, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, uid)
	return f.markErr
}
func (f *fakeClient) Logout() { f.mu.Lock(); f.logouts++; f.mu.Unlock() }

// ------------------------------------------------------------ handler harness

type fakeSecrets struct{ ws map[string]string }

func (s *fakeSecrets) AppSetting(context.Context, string) (string, error) { return "", nil }
func (s *fakeSecrets) WorkspaceSetting(_ context.Context, ws, key string) (string, bool, error) {
	v, ok := s.ws[ws+"/"+key]
	return v, ok, nil
}
func (s *fakeSecrets) SetWorkspaceSetting(_ context.Context, ws, key, value string) error {
	s.ws[ws+"/"+key] = value
	return nil
}

func resolver(t *testing.T, ws string, kv map[string]string) *secrets.Resolver {
	t.Helper()
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: "t18-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := secrets.NewResolver(e, &fakeSecrets{ws: map[string]string{}}, func(string) string { return "" })
	for k, v := range kv {
		if err := r.Set(context.Background(), ws, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

var now0 = time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)

func settings() config.OutreachSettings {
	return config.OutreachSettings{
		InboundPollEnabled: true, InboundPollInterval: 900, InboundMaxFetch: 100, InboundMaxConsecutiveFailures: 3,
		InboundLookbackDays: 3, CircuitMinSends: 20, BouncePauseRate: 0.05, ComplaintPauseRate: 0.003, SoftBounceMax: 3,
	}
}

func imapKV() map[string]string {
	return map[string]string{"IMAP_HOST": "imap.test.invalid", "IMAP_USER": "bounces@sender.example", "IMAP_PASSWORD": imapPassword}
}

func newHandler(t *testing.T, pool *pgxpool.Pool, ws string, fc *fakeClient) (*outreachinbound.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return &outreachinbound.Handler{
		Pool: pool, Secrets: resolver(t, ws, imapKV()), Automations: true, Outreach: settings(),
		NewClient: func(outreachcfg.IMAP) imapclient.Client { return fc },
		Now:       func() time.Time { return now0 },
		Log:       slog.New(slog.NewTextHandler(&logs, nil)),
	}, &logs
}

func tick(t *testing.T, h *outreachinbound.Handler, ws string) {
	t.Helper()
	c := &queue.Claim{WorkspaceID: ws, Payload: json.RawMessage(`{"workspace_id":"` + ws + `"}`)}
	if err := h.Handle(context.Background(), c); err != nil {
		t.Fatalf("handle must not fail the worker: %v", err)
	}
}

func sched(t *testing.T, pool *pgxpool.Pool, ws string) *outreachstore.InboundSchedule {
	t.Helper()
	s, err := outreachstore.GetInboundSchedule(context.Background(), pool, ws)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func jobsFor(t *testing.T, pool *pgxpool.Pool, ws string) []string {
	t.Helper()
	var keys []string
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT fire_key FROM jobs WHERE workspace_id=$1 AND type='outreach_inbound_poll' ORDER BY id`, ws)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			_ = rows.Scan(&k)
			keys = append(keys, k)
		}
	})
	return keys
}

// ------------------------------------------------------------------ Process

type corpusExpected struct {
	PythonError *string `json:"python_error"`
	Records     []struct {
		Diagnostic        string `json:"diagnostic"`
		Kind              string `json:"kind"`
		OriginalMessageID string `json:"original_message_id"`
		Recipient         string `json:"recipient"`
	} `json:"records"`
	SourceMessageID string `json:"source_message_id"`
}

// Every recorded Python parse outcome must land in the ledger as _process_one
// would write it: first record whose original Message-ID is ours is the match,
// else unmatched (records, no match) or unknown (no records, Python raising
// included: Go records it so it is never retried forever).
func TestProcessMatchesPythonCorpusOutcomes(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	files, _ := filepath.Glob("../../testdata/dsn/*.eml")
	if len(files) < 50 {
		t.Fatalf("corpus: %d files", len(files))
	}
	var man struct {
		Files []struct {
			Name      string `json:"name"`
			GoBounded bool   `json:"go_bounded"`
		} `json:"files"`
	}
	mb, err := os.ReadFile("../dsnparse/testdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatal(err)
	}
	bounded := map[string]bool{}
	for _, f := range man.Files {
		if f.GoBounded {
			bounded[f.Name] = true // documented D6 divergence (parse bounds); covered in dsnparse
		}
	}
	matched := 0
	for i, f := range files {
		if bounded[strings.TrimSuffix(filepath.Base(f), ".eml")] || bounded[filepath.Base(f)] {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var exp corpusExpected
		jb, err := os.ReadFile(strings.TrimSuffix(f, ".eml") + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(jb, &exp); err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		ws := newWS(t, pool) // corpus files share source Message-IDs; the ledger key is per workspace
		uid := strconv.Itoa(1000 + i)
		var want ledgerRow
		var wantSend *seeded
		if exp.PythonError == nil && len(exp.Records) > 0 {
			rec := exp.Records[0]
			want = ledgerRow{Kind: rec.Kind, Recipient: rec.Recipient, Diagnostic: rec.Diagnostic}
			for _, r := range exp.Records {
				if strings.TrimSpace(r.OriginalMessageID) != "" {
					s := seedSend(t, pool, ws, "seed-"+uid+"@example.com", r.OriginalMessageID+"", 1000+i)
					wantSend = &s
					want = ledgerRow{Kind: r.Kind, Recipient: r.Recipient, Diagnostic: r.Diagnostic}
					break
				}
			}
			if wantSend == nil {
				want.Kind = "unmatched"
			} else if want.Recipient == "" {
				want.Recipient = "seed-" + uid + "@example.com"
			}
		} else {
			want = ledgerRow{Kind: "unknown", Diagnostic: "no bounce record"}
		}
		process(t, pool, ws, uid, raw)
		var got *ledgerRow
		for _, r := range ledger(t, pool, ws) {
			r := r
			if r.UID == uid {
				got = &r
			}
		}
		if got == nil {
			t.Fatalf("%s: no ledger row", name)
		}
		if got.Kind != want.Kind {
			t.Errorf("%s: kind %q want %q", name, got.Kind, want.Kind)
		}
		if got.Recipient != truncate(want.Recipient, 320) || got.Diagnostic != truncate(want.Diagnostic, 500) {
			if want.Kind == "unmatched" || want.Kind == "unknown" || wantSend != nil {
				// the recipient may fall back to the send's address only on a match
				if !(wantSend != nil && want.Recipient == "") {
					t.Errorf("%s: recipient/diag %q/%q want %q/%q", name, got.Recipient, got.Diagnostic, want.Recipient, want.Diagnostic)
				}
			}
		}
		if wantSend != nil {
			matched++
			if got.Matched == nil || *got.Matched != wantSend.sendID {
				t.Errorf("%s: matched_send_id %v want %d", name, got.Matched, wantSend.sendID)
			}
			if want.Kind == "hard" || want.Kind == "complaint" {
				if st := sendStatus(t, pool, ws, wantSend.sendID); st != "bounced" {
					t.Errorf("%s: send status %q", name, st)
				}
			}
		} else if got.Kind == "unmatched" && got.Matched != nil {
			// recipient-only fallback is recorded, never acted on
			if st := sendStatus(t, pool, ws, *got.Matched); st == "bounced" {
				t.Errorf("%s: fallback match was acted on", name)
			}
		}
	}
	if matched < 10 {
		t.Fatalf("only %d corpus messages matched a send; corpus coverage too thin", matched)
	}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func TestSpoofedDSNWithoutMessageIDNotActedOn(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	victim := seedSend(t, pool, ws, "victim@example.com", "<ours-1@sender.example>", 1)
	// recipient matches our real send, but the original Message-ID is not ours
	process(t, pool, ws, "1", dsn("<spoof-1@evil.example>", "<not-ours@evil.example>", "victim@example.com", "5.1.1", "550 user unknown"))
	// and one with no original Message-ID at all
	process(t, pool, ws, "2", []byte("From: MAILER-DAEMON@evil.example\r\nTo: bounces@sender.example\r\nMessage-ID: <spoof-2@evil.example>\r\nContent-Type: text/plain\r\n\r\n550 5.1.1 victim@example.com user unknown does not exist\r\n"))
	rows := ledger(t, pool, ws)
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Kind != "unmatched" && r.Kind != "unknown" {
			t.Errorf("uid %s acted kind %q", r.UID, r.Kind)
		}
	}
	if st := sendStatus(t, pool, ws, victim.sendID); st != "sent" {
		t.Errorf("send status %q", st)
	}
	if b, _ := seqCounts(t, pool, ws, victim.seq); b != 0 {
		t.Errorf("breaker counter bumped: %d", b)
	}
	if _, _, ok := suppression(t, pool, ws, "victim@example.com"); ok {
		t.Errorf("spoofed DSN suppressed an address")
	}
	if s, _ := enrStatus(t, pool, ws, victim.enr); s == "bounced" {
		t.Errorf("enrollment bounced by spoof")
	}
}

func TestSpoofedDSNOtherTenantMessageIDNotActedOn(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	wsA, wsB := newWS(t, pool), newWS(t, pool)
	a := seedSend(t, pool, wsA, "a@example.com", "<tenant-a-1@sender.example>", 1)
	process(t, pool, wsB, "1", dsn("<d@x>", "<tenant-a-1@sender.example>", "a@example.com", "5.1.1", "550 no"))
	if st := sendStatus(t, pool, wsA, a.sendID); st != "sent" {
		t.Fatalf("tenant A send changed: %s", st)
	}
	if rows := ledger(t, pool, wsB); len(rows) != 1 || rows[0].Kind != "unmatched" || rows[0].Matched != nil {
		t.Fatalf("B ledger: %+v", rows)
	}
}

func TestReprocessingNeverDoubleCounts(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "gone@example.com", "<ours-2@sender.example>", 1)
	raw := dsn("<dsn-2@mx.example>", "<ours-2@sender.example>", "gone@example.com", "5.1.1", "550 user unknown")
	for i := 0; i < 4; i++ {
		process(t, pool, ws, "9", raw)
	}
	if b, _ := seqCounts(t, pool, ws, s.seq); b != 1 {
		t.Fatalf("bounce_count %d, want 1", b)
	}
	if rows := ledger(t, pool, ws); len(rows) != 1 || rows[0].Kind != "hard" || rows[0].Matched == nil || *rows[0].Matched != s.sendID {
		t.Fatalf("ledger: %+v", rows)
	}
	if r, _, ok := suppression(t, pool, ws, "gone@example.com"); !ok || r != "bounce" {
		t.Fatalf("suppression %q %v", r, ok)
	}
	if st, e := enrStatus(t, pool, ws, s.enr); st != "bounced" || e != "hard_bounce" {
		t.Fatalf("enrollment %q %q", st, e)
	}
}

// Two pollers (or a restart racing a retry) handling the same message: the ledger
// row and ApplyBounce commit together, so exactly one application lands.
func TestConcurrentBounceAppliedExactlyOnce(t *testing.T) {
	pool := testpg.NewAppPool(t, 16)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "race@example.com", "<ours-3@sender.example>", 1)
	raw := dsn("<dsn-3@mx.example>", "<ours-3@sender.example>", "race@example.com", "5.1.1", "550 user unknown")
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx := context.Background()
			errs <- tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				return outreachinbound.Process(ctx, tx, ws, "42", "5", raw, th)
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("process: %v", err)
		}
	}
	if b, _ := seqCounts(t, pool, ws, s.seq); b != 1 {
		t.Fatalf("bounce_count %d, want exactly 1", b)
	}
	if rows := ledger(t, pool, ws); len(rows) != 1 {
		t.Fatalf("ledger rows: %d", len(rows))
	}
}

// A failure after the ledger insert must roll the ledger row back too, so the
// next attempt applies the bounce (no lost bounce, no double count).
func TestLedgerAndBounceShareOneTransaction(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "tx@example.com", "<ours-4@sender.example>", 1)
	raw := dsn("<dsn-4@mx.example>", "<ours-4@sender.example>", "tx@example.com", "5.1.1", "550 no")
	ctx := context.Background()
	boom := errors.New("boom")
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		if err := outreachinbound.Process(ctx, tx, ws, "42", "6", raw, th); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if rows := ledger(t, pool, ws); len(rows) != 0 {
		t.Fatalf("ledger survived rollback: %+v", rows)
	}
	process(t, pool, ws, "6", raw)
	if b, _ := seqCounts(t, pool, ws, s.seq); b != 1 {
		t.Fatalf("bounce_count %d", b)
	}
}

// seedMoreSend adds a further sent send (another step) to the seeded enrollment.
func seedMoreSend(t *testing.T, pool *pgxpool.Pool, ws string, s seeded, to, mid string, step int) {
	t.Helper()
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		e32 := int32(s.enr)
		idem := fmt.Sprintf("idem-%s-%d", mid, time.Now().UnixNano())
		if ok, err := outreachstore.InsertInFlight(ctx, tx, outreachstore.SendIn{WorkspaceID: ws, SequenceID: s.seq, EnrollmentID: &e32,
			StepNumber: int32(step), ToEmail: to, Subject: "hi", IdempotencyKey: idem}); err != nil || !ok {
			t.Fatalf("in flight: %v %v", ok, err)
		}
		now := time.Now()
		if err := outreachstore.FinalizeSend(ctx, tx, ws, idem, outreachstore.Finalize{Status: "sent", MessageID: mid, SentAt: &now}); err != nil {
			t.Fatal(err)
		}
	})
}

// S-L14: soft DSNs citing one send count once; the cap needs soft bounces on distinct sends.
func TestSoftBounceForgedForOneSendDoesNotSuppress(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "forge@example.com", "<ours-6@sender.example>", 1)
	for i := 1; i <= 4; i++ {
		process(t, pool, ws, strconv.Itoa(i), dsn(fmt.Sprintf("<forge-%d@mx.example>", i), "<ours-6@sender.example>", "forge@example.com", "4.2.2", "452 mailbox full"))
	}
	if _, _, supp := suppression(t, pool, ws, "forge@example.com"); supp {
		t.Fatal("forged soft DSNs for one send suppressed the recipient")
	}
	if st, _ := enrStatus(t, pool, ws, s.enr); st == "bounced" {
		t.Fatal("enrollment bounced by forged soft DSNs")
	}
}

func TestSoftBounceCapSuppresses(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "full@example.com", "<ours-5@sender.example>", 1)
	seedMoreSend(t, pool, ws, s, "full@example.com", "<ours-5b@sender.example>", 2)
	seedMoreSend(t, pool, ws, s, "full@example.com", "<ours-5c@sender.example>", 3)
	for i := 1; i <= 3; i++ {
		orig := []string{"", "<ours-5@sender.example>", "<ours-5b@sender.example>", "<ours-5c@sender.example>"}[i]
		raw := dsn(fmt.Sprintf("<soft-%d@mx.example>", i), orig, "full@example.com", "4.2.2", "452 mailbox full")
		process(t, pool, ws, strconv.Itoa(i), raw)
		_, _, supp := suppression(t, pool, ws, "full@example.com")
		st, _ := enrStatus(t, pool, ws, s.enr)
		if i < 3 && (supp || st == "bounced") {
			t.Fatalf("after %d soft bounces: suppressed=%v enrollment=%s", i, supp, st)
		}
	}
	if r, locked, ok := suppression(t, pool, ws, "full@example.com"); !ok || r != "bounce" || locked {
		t.Fatalf("suppression %q locked=%v ok=%v", r, locked, ok)
	}
	if st, e := enrStatus(t, pool, ws, s.enr); st != "bounced" || e != "soft_bounce_max" {
		t.Fatalf("enrollment %q %q", st, e)
	}
	// the send that reached the cap is the one marked bounced, not the first
	if sendStatus(t, pool, ws, s.sendID) != "sent" {
		t.Fatalf("first send must stay sent")
	}
}

func TestComplaintLocksSuppression(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "ann@example.com", "<orig-0015@contract.example.com>", 1)
	raw, err := os.ReadFile("../../testdata/dsn/arf_abuse_original_rcpt.eml")
	if err != nil {
		t.Fatal(err)
	}
	process(t, pool, ws, "15", raw)
	if r, locked, ok := suppression(t, pool, ws, "ann@example.com"); !ok || r != "complaint" || !locked {
		t.Fatalf("suppression %q locked=%v ok=%v", r, locked, ok)
	}
	if _, c := seqCounts(t, pool, ws, s.seq); c != 1 {
		t.Fatalf("complaint_count %d", c)
	}
	if st, e := enrStatus(t, pool, ws, s.enr); st != "bounced" || e != "complaint" {
		t.Fatalf("enrollment %q %q", st, e)
	}
}

// ------------------------------------------------------------------ Handler

func TestMissingCredsDisablesSchedule(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	next := now0.Add(time.Hour)
	if err := outreachstore.UpsertInboundSchedule(context.Background(), pool, ws, outreachstore.InboundScheduleUpsert{Enabled: true, NextPollAt: &next}); err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{}
	h, _ := newHandler(t, pool, ws, fc)
	h.Secrets = resolver(t, ws, map[string]string{"IMAP_HOST": "imap.test.invalid"}) // no user/password
	tick(t, h, ws)
	s := sched(t, pool, ws)
	if s == nil || s.Enabled || s.NextPollAt != nil {
		t.Fatalf("schedule not disabled: %+v", s)
	}
	if fc.connects != 0 {
		t.Fatal("connected without credentials")
	}
	if len(jobsFor(t, pool, ws)) != 0 {
		t.Fatal("job enqueued for a disabled schedule")
	}
}

func TestAutoDisableAtCap(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	fc := &fakeClient{connectErr: errors.New("login failed for " + imapPassword)}
	h, logs := newHandler(t, pool, ws, fc)
	for i, wantBackoff := range []time.Duration{900 * time.Second, 1800 * time.Second} {
		tick(t, h, ws)
		s := sched(t, pool, ws)
		if s == nil || !s.Enabled || s.ConsecutiveFailures != i+1 || s.NextPollAt == nil || !s.NextPollAt.Equal(now0.Add(wantBackoff)) {
			t.Fatalf("tick %d: %+v", i+1, s)
		}
		if keys := jobsFor(t, pool, ws); len(keys) != i+1 || keys[i] != "inbound:"+ws+":"+now0.Add(wantBackoff).Format("2006-01-02T15:04:05")+"+00:00" {
			t.Fatalf("tick %d jobs: %v", i+1, keys)
		}
	}
	tick(t, h, ws) // third failure reaches the cap of 3
	s := sched(t, pool, ws)
	if s.Enabled || s.NextPollAt != nil || s.ConsecutiveFailures != 3 {
		t.Fatalf("not auto-disabled: %+v", s)
	}
	if n := len(jobsFor(t, pool, ws)); n != 2 {
		t.Fatalf("a job was enqueued after auto-disable: %d", n)
	}
	if fc.logouts != 3 {
		t.Fatalf("logouts %d", fc.logouts)
	}
	if strings.Contains(logs.String(), imapPassword) {
		t.Fatal("password leaked into logs")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	two := 2
	if err := outreachstore.UpsertInboundSchedule(context.Background(), pool, ws, outreachstore.InboundScheduleUpsert{Enabled: true, ConsecutiveFailures: &two}); err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{uidValidity: "9001"}
	h, _ := newHandler(t, pool, ws, fc)
	tick(t, h, ws)
	s := sched(t, pool, ws)
	if s.ConsecutiveFailures != 0 || !s.Enabled || s.UIDValidity != "9001" || s.NextPollAt == nil || !s.NextPollAt.Equal(now0.Add(900*time.Second)) {
		t.Fatalf("%+v", s)
	}
	if n := len(jobsFor(t, pool, ws)); n != 1 {
		t.Fatalf("jobs %d", n)
	}
}

func TestNextPollEnqueuedOnce(t *testing.T) {
	pool := testpg.NewAppPool(t, 20)
	ws := newWS(t, pool)
	cfg := outreachinbound.ScheduleConfig{Outreach: settings()}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := outreachinbound.Reschedule(context.Background(), pool, ws, false, nil, now0, cfg); err != nil {
				t.Errorf("reschedule: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	key := outreachinbound.FireKey(ws, now0.Add(900*time.Second))
	var n int
	var executor, payload string
	var prio, maxRetries int
	var dueOK bool
	inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE fire_key=$1`, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT executor, payload::text, priority, max_retries, next_run_at = ($2::timestamptz AT TIME ZONE 'UTC')
			FROM jobs WHERE fire_key=$1`, key, now0.Add(900*time.Second)).Scan(&executor, &payload, &prio, &maxRetries, &dueOK); err != nil {
			t.Fatal(err)
		}
	})
	if n != 1 {
		t.Fatalf("jobs for fire key: %d", n)
	}
	var p map[string]string
	_ = json.Unmarshal([]byte(payload), &p)
	if executor != "go" || prio != 1 || maxRetries != 3 || !dueOK || p["workspace_id"] != ws || p["fire_key"] != key {
		t.Fatalf("job: exec=%s prio=%d retries=%d due=%v payload=%s", executor, prio, maxRetries, dueOK, payload)
	}
}

// The cohort and GO_EXECUTOR_OUTREACH_INBOUND are retired (plan 15 cutover): every next poll is go.
func TestRescheduleExecutorIsAlwaysGo(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	in, out := newWS(t, pool), newWS(t, pool)
	o := settings()
	o.GoExecutorInbound = true
	cfg := outreachinbound.ScheduleConfig{Outreach: o, Executor: config.ExecutorSettings{Enabled: true, Workspaces: map[string]bool{in: true}}}
	for _, ws := range []string{in, out} {
		if err := outreachinbound.Reschedule(context.Background(), pool, ws, false, nil, now0, cfg); err != nil {
			t.Fatal(err)
		}
	}
	for ws, want := range map[string]string{in: "go", out: "go"} {
		var ex string
		inTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
			if err := tx.QueryRow(ctx, `SELECT executor FROM jobs WHERE workspace_id=$1 AND type='outreach_inbound_poll'`, ws).Scan(&ex); err != nil {
				t.Fatal(err)
			}
		})
		if ex != want {
			t.Errorf("%s executor %q want %q", ws, ex, want)
		}
	}
}

func TestMarkSeenFailureIgnored(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	seedSend(t, pool, ws, "m@example.com", "<ours-6@sender.example>", 1)
	fc := &fakeClient{uids: []string{"1"}, markErr: errors.New("store failed"),
		msgs: map[string][]byte{"1": dsn("<d6@x>", "<ours-6@sender.example>", "m@example.com", "5.1.1", "550 no")}}
	h, _ := newHandler(t, pool, ws, fc)
	tick(t, h, ws)
	if rows := ledger(t, pool, ws); len(rows) != 1 || rows[0].Kind != "hard" {
		t.Fatalf("ledger %+v", rows)
	}
	if s := sched(t, pool, ws); s.ConsecutiveFailures != 0 {
		t.Fatalf("mark_seen failure counted as a failed tick: %+v", s)
	}
	if len(fc.seen) != 1 {
		t.Fatalf("seen %v", fc.seen)
	}
}

func TestLogoutAlwaysCalled(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	for name, fc := range map[string]*fakeClient{
		"ok":      {},
		"connect": {connectErr: errors.New("dial refused")},
		"select":  {selectErr: errors.New("no such folder")},
		"fetch":   {uids: []string{"1"}, fetchErrs: map[string][]error{"1": {errors.New("io")}}},
	} {
		h, _ := newHandler(t, pool, ws, fc)
		tick(t, h, ws)
		if fc.logouts != 1 {
			t.Errorf("%s: logouts %d", name, fc.logouts)
		}
	}
}

func TestMaxFetchCap(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	msgs := map[string][]byte{}
	var uids []string
	for i := 1; i <= 5; i++ {
		u := strconv.Itoa(i)
		uids = append(uids, u)
		msgs[u] = []byte("Message-ID: <m" + u + "@x>\r\nContent-Type: text/plain\r\n\r\nnewsletter\r\n")
	}
	fc := &fakeClient{uids: uids, msgs: msgs}
	h, _ := newHandler(t, pool, ws, fc)
	h.Outreach.InboundMaxFetch = 2
	tick(t, h, ws)
	if fc.searchMax != 2 {
		t.Fatalf("search max %d", fc.searchMax)
	}
	if len(fc.fetched) != 2 || len(ledger(t, pool, ws)) != 2 {
		t.Fatalf("fetched %v ledger %d (fake returned 5 uids)", fc.fetched, len(ledger(t, pool, ws)))
	}
	if want := now0.Add(-3 * 24 * time.Hour); !fc.searchSince.Equal(want) {
		t.Fatalf("since %v want %v", fc.searchSince, want)
	}
}

func TestOnePoisonMessageDoesNotStopBatch(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	ok := func(u string) []byte {
		return []byte("Message-ID: <m" + u + "@x>\r\nContent-Type: text/plain\r\n\r\nhello\r\n")
	}
	fc := &fakeClient{uids: []string{"1", "2", "3"}, msgs: map[string][]byte{"1": ok("1"), "3": ok("3")},
		fetchErrs: map[string][]error{"2": {errors.New("fetch exploded")}}}
	h, _ := newHandler(t, pool, ws, fc)
	tick(t, h, ws)
	rows := ledger(t, pool, ws)
	if len(rows) != 2 || rows[0].UID != "1" || rows[1].UID != "3" {
		t.Fatalf("ledger %+v", rows)
	}
	if s := sched(t, pool, ws); s.ConsecutiveFailures != 0 {
		t.Fatalf("a poison message (Python: empty fetch) must not fail the tick: %+v", s)
	}
	if fmt.Sprint(fc.seen) != "[1 3]" {
		t.Fatalf("seen %v", fc.seen)
	}
}

func TestNonASCIIOrUnparseableRecordedNotRetried(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	raw := []byte("From: x@y.example\r\nMessage-ID: <na@x>\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=B\r\n\r\n--B\r\nContent-Type: message/delivery-status\r\n\r\nFinal-Recipient: rfc822; r\xc3\xa9@example.com\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\n--B--\r\n")
	fc := &fakeClient{uids: []string{"1"}, msgs: map[string][]byte{"1": raw}}
	h, _ := newHandler(t, pool, ws, fc)
	tick(t, h, ws)
	if rows := ledger(t, pool, ws); len(rows) != 1 {
		t.Fatalf("ledger %+v", rows)
	}
	if s := sched(t, pool, ws); s.ConsecutiveFailures != 0 || len(fc.seen) != 1 {
		t.Fatalf("not treated as processed: %+v seen=%v", s, fc.seen)
	}
}

func TestOversizedMessageMarkedSeenNotFailed(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	fc := &fakeClient{uids: []string{"1"}, fetchErrs: map[string][]error{"1": {imapclient.ErrOversized}}}
	h, _ := newHandler(t, pool, ws, fc)
	tick(t, h, ws)
	if fmt.Sprint(fc.seen) != "[1]" {
		t.Fatalf("oversized message not marked seen: %v", fc.seen)
	}
	if s := sched(t, pool, ws); s.ConsecutiveFailures != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestReconnectAfterBudgetExceeded(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	ok := []byte("Message-ID: <r2@x>\r\nContent-Type: text/plain\r\n\r\nhello\r\n")
	fc := &fakeClient{uids: []string{"1", "2"}, msgs: map[string][]byte{"2": ok}, uidValidity2: "888",
		fetchErrs: map[string][]error{"1": {imapclient.ErrBudgetExceeded}}}
	h, _ := newHandler(t, pool, ws, fc)
	tick(t, h, ws)
	if fc.connects != 2 {
		t.Fatalf("connects %d, want a reconnect", fc.connects)
	}
	if rows := ledger(t, pool, ws); len(rows) != 1 || rows[0].UID != "2" {
		t.Fatalf("ledger %+v", rows)
	}
	if s := sched(t, pool, ws); s.ConsecutiveFailures != 0 {
		t.Fatalf("%+v", s)
	}
	if sc := sched(t, pool, ws); sc.UIDValidity != "888" {
		t.Fatalf("uidvalidity after reconnect not kept: %q", sc.UIDValidity)
	}
	if fc.logouts < 2 {
		t.Fatalf("logouts %d: the dropped connection must be logged out", fc.logouts)
	}
}

func TestBlockedHostFailsTickNotWorker(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	kv := imapKV()
	kv["IMAP_HOST"] = "127.0.0.1"
	var logs bytes.Buffer
	h := &outreachinbound.Handler{
		Pool: pool, Secrets: resolver(t, ws, kv), Automations: true, Outreach: settings(),
		Dial: egress.NewDialContext("production", nil), // production constructor path: imapclient.New
		Now:  func() time.Time { return now0 }, Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	tick(t, h, ws)
	s := sched(t, pool, ws)
	if s == nil || s.ConsecutiveFailures != 1 || !s.Enabled {
		t.Fatalf("%+v", s)
	}
	if strings.Contains(logs.String(), imapPassword) {
		t.Fatal("password leaked")
	}
}

// ------------------------------------------------- real in-process IMAP server

func TestPollAgainstInProcessIMAPServer(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "real@example.com", "<ours-7@sender.example>", 1)

	mem := imapmemserver.New()
	u := imapmemserver.NewUser("bounces@sender.example", imapPassword)
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	body := string(dsn("<d7@mx.example>", "<ours-7@sender.example>", "real@example.com", "5.1.1", "550 user unknown"))
	if _, err := u.Append("INBOX", &lit{strings.NewReader(body), int64(len(body))}, &imap.AppendOptions{Time: now0}); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	kv := map[string]string{"IMAP_HOST": host, "IMAP_PORT": port, "IMAP_USE_SSL": "0", "IMAP_USER": "bounces@sender.example", "IMAP_PASSWORD": imapPassword}
	h := &outreachinbound.Handler{
		Pool: pool, Secrets: resolver(t, ws, kv), Automations: true, Outreach: settings(),
		NewClient: func(c outreachcfg.IMAP) imapclient.Client {
			return imapclient.NewWithOptions(c, (&net.Dialer{}).DialContext, 10*time.Second, imapclient.Options{PlainHosts: []string{host}})
		},
		Now: func() time.Time { return now0.Add(time.Hour) },
	}
	tick(t, h, ws)
	if rows := ledger(t, pool, ws); len(rows) != 1 || rows[0].Kind != "hard" {
		t.Fatalf("ledger %+v", rows)
	}
	if st := sendStatus(t, pool, ws, s.sendID); st != "bounced" {
		t.Fatalf("send %s", st)
	}
	if sc := sched(t, pool, ws); sc.ConsecutiveFailures != 0 || sc.UIDValidity == "" {
		t.Fatalf("%+v", sc)
	}
	// second tick: the message is \Seen now, nothing to do, still no double count
	tick(t, h, ws)
	if b, _ := seqCounts(t, pool, ws, s.seq); b != 1 {
		t.Fatalf("bounce_count %d", b)
	}
}

type lit struct {
	*strings.Reader
	n int64
}

func (l *lit) Size() int64 { return l.n }

func TestNULBytesStrippedBeforeLedgerInsert(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "nul@example.com", "<ours-8@sender.example>", 1)
	raw := dsn("<d\x008@mx.example>", "<ours-8@sender.example>", "nul@example.com", "5.1.1", "550 user\x00 unknown")
	raw = bytes.Replace(raw, []byte("Subject: Undelivered"), []byte("Subject: Unde\x00livered"), 1)
	process(t, pool, ws, "1", raw)
	// unmatched path too (recipient + diagnostic carry NUL)
	process(t, pool, ws, "2", dsn("<d9@mx.example>", "<nope@x>", "nu\x00l2@example.com", "5.1.1", "550 a\x00b"))
	rows := ledger(t, pool, ws)
	if len(rows) != 2 {
		t.Fatalf("ledger %+v", rows)
	}
	for _, r := range rows {
		if strings.ContainsRune(r.Diagnostic+r.Recipient+r.SourceMID, 0) {
			t.Errorf("NUL stored: %+v", r)
		}
	}
	if sendStatus(t, pool, ws, s.sendID) != "bounced" {
		t.Errorf("matched NUL message not applied")
	}
}

// Plan 09 final security M1: anyone who saw one email knows its Message-ID. Repeated or forged
// DSN/ARF mail citing it must not move the sequence's circuit-breaker counters more than once.
func TestRepeatedHardBounceForSameSendCountsOnce(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "gone@example.com", "<ours-m1a@sender.example>", 1)
	for i := 1; i <= 3; i++ {
		raw := dsn(fmt.Sprintf("<forged-%d@mx.example>", i), "<ours-m1a@sender.example>", "gone@example.com", "5.1.1", "550 user unknown")
		process(t, pool, ws, strconv.Itoa(i), raw)
	}
	if b, _ := seqCounts(t, pool, ws, s.seq); b != 1 {
		t.Fatalf("bounce_count %d, want 1", b)
	}
}

func TestRepeatedComplaintForSameSendCountsOnce(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "ann@example.com", "<orig-0015@contract.example.com>", 1)
	raw, err := os.ReadFile("../../testdata/dsn/arf_abuse_original_rcpt.eml")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		forged := strings.Replace(string(raw), "<arf-0015@isp.example.net>", fmt.Sprintf("<arf-forged-%d@isp.example.net>", i), 1)
		process(t, pool, ws, strconv.Itoa(20+i), []byte(forged))
	}
	if _, c := seqCounts(t, pool, ws, s.seq); c != 1 {
		t.Fatalf("complaint_count %d, want 1", c)
	}
}

// A DSN whose recipient is not the send's recipient is never counted by the breaker.
func TestBounceForOtherRecipientNotCounted(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "gone@example.com", "<ours-m1c@sender.example>", 1)
	raw := dsn("<forged-other@mx.example>", "<ours-m1c@sender.example>", "someone-else@example.org", "5.1.1", "550 user unknown")
	process(t, pool, ws, "31", raw)
	if b, _ := seqCounts(t, pool, ws, s.seq); b != 0 {
		t.Fatalf("bounce_count %d, want 0 for a mismatched recipient", b)
	}
	// the genuine bounce afterwards still counts, once
	process(t, pool, ws, "32", dsn("<real@mx.example>", "<ours-m1c@sender.example>", "Gone@Example.com", "5.1.1", "550 user unknown"))
	if b, _ := seqCounts(t, pool, ws, s.seq); b != 1 {
		t.Fatalf("bounce_count %d, want 1", b)
	}
}

// Plan 09 final security L5: like the send handler, a payload naming another workspace than the job's
// own row is a permanent failure and polls nothing.
func TestPollRejectsPayloadWorkspaceMismatch(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	fc := &fakeClient{}
	h, _ := newHandler(t, pool, ws, fc)
	c := &queue.Claim{WorkspaceID: "some-other-ws", Payload: json.RawMessage(`{"workspace_id":"` + ws + `"}`)}
	err := h.Handle(context.Background(), c)
	var perm *queue.PermanentError
	if !errors.As(err, &perm) || fc.connects != 0 {
		t.Fatalf("err=%v connects=%d", err, fc.connects)
	}
}

// Review follow-up M1: the dedupe is over the whole ledger, not a bounded window. 600 forged DSNs naming
// another recipient must not push the first genuine report out of it.
func TestBreakerDedupeSurvivesLedgerFlood(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	s := seedSend(t, pool, ws, "gone@example.com", "<ours-flood@sender.example>", 1)
	process(t, pool, ws, "1", dsn("<real-0@mx.example>", "<ours-flood@sender.example>", "gone@example.com", "5.1.1", "550 user unknown"))
	for i := 0; i < 600; i++ {
		process(t, pool, ws, strconv.Itoa(100+i), dsn(fmt.Sprintf("<flood-%d@mx.example>", i), "<ours-flood@sender.example>",
			fmt.Sprintf("other%d@example.org", i%7), "5.1.1", "550 user unknown"))
	}
	for i := 0; i < 3; i++ {
		process(t, pool, ws, strconv.Itoa(1000+i), dsn(fmt.Sprintf("<repeat-%d@mx.example>", i), "<ours-flood@sender.example>", "gone@example.com", "5.1.1", "550 user unknown"))
	}
	if b, _ := seqCounts(t, pool, ws, s.seq); b != 1 {
		t.Fatalf("bounce_count %d, want 1", b)
	}
}

// ---- global mailbox shared by several workspaces (final-fix follow-up M2)

// globalHandler resolves IMAP from the global/env settings (no per-workspace IMAP keys), like every workspace
// that never configured its own mailbox.
func globalHandler(t *testing.T, pool *pgxpool.Pool, fc *fakeClient) *outreachinbound.Handler {
	t.Helper()
	e, err := secrets.NewEnvelope(secrets.Config{MasterKey: "t18-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := imapKV()
	r := secrets.NewResolver(e, &fakeSecrets{ws: map[string]string{}}, func(k string) string { return env[k] })
	return &outreachinbound.Handler{
		Pool: pool, Secrets: r, Automations: true, Outreach: settings(),
		NewClient: func(outreachcfg.IMAP) imapclient.Client { return fc },
		Now:       func() time.Time { return now0 },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestGlobalMailboxRoutesEachMessageToItsOwnerWorkspace(t *testing.T) {
	pool := testpg.NewAppPool(t, 6)
	a, b := newWS(t, pool), newWS(t, pool)
	sa := seedSend(t, pool, a, "gone-a@example.com", "<ours-ga@sender.example>", 1)
	sb := seedSend(t, pool, b, "gone-b@example.com", "<ours-gb@sender.example>", 1)
	fc := &fakeClient{uids: []string{"1", "2", "3"}, msgs: map[string][]byte{
		"1": dsn("<d1@mx.example>", "<ours-gb@sender.example>", "gone-b@example.com", "5.1.1", "550 user unknown"),
		"2": dsn("<d2@mx.example>", "<ours-ga@sender.example>", "gone-a@example.com", "5.1.1", "550 user unknown"),
		"3": dsn("<d3@mx.example>", "<nobody@sender.example>", "x@example.com", "5.1.1", "550 user unknown"),
	}}
	tick(t, globalHandler(t, pool, fc), a) // only A's poll runs: B has no poll of its own this tick
	if r, _, ok := suppression(t, pool, b, "gone-b@example.com"); !ok || r != "bounce" {
		t.Fatalf("B's bounce lost: %q %v", r, ok)
	}
	if r, _, ok := suppression(t, pool, a, "gone-a@example.com"); !ok || r != "bounce" {
		t.Fatalf("A's bounce lost: %q %v", r, ok)
	}
	if bc, _ := seqCounts(t, pool, b, sb.seq); bc != 1 {
		t.Fatalf("B bounce_count %d", bc)
	}
	if bc, _ := seqCounts(t, pool, a, sa.seq); bc != 1 {
		t.Fatalf("A bounce_count %d", bc)
	}
	if st, _ := enrStatus(t, pool, b, sb.enr); st != "bounced" {
		t.Fatalf("B enrollment %s", st)
	}
	// the ledger rows live in the owning workspace; the unmatched one in the poller's
	if rows := ledger(t, pool, b); len(rows) != 1 || rows[0].Matched == nil || *rows[0].Matched != sb.sendID {
		t.Fatalf("B ledger %+v", rows)
	}
	if rows := ledger(t, pool, a); len(rows) != 2 {
		t.Fatalf("A ledger %+v", rows)
	}
	if len(fc.seen) != 3 {
		t.Fatalf("marked seen %v", fc.seen)
	}
}

// One poll of the shared mailbox at a time: a second workspace's tick while the lease is held does not even connect.
func TestGlobalMailboxPollIsSingleFlight(t *testing.T) {
	pool := testpg.NewAppPool(t, 6)
	a := newWS(t, pool)
	fc := &fakeClient{}
	h := globalHandler(t, pool, fc)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, outreachinbound.GlobalLeaseKey(outreachcfg.IMAP{Host: "imap.test.invalid", Port: 993, User: "bounces@sender.example"})); err != nil {
		t.Fatal(err)
	}
	tick(t, h, a)
	if fc.connects != 0 {
		t.Fatalf("polled while another workspace holds the mailbox lease (%d connects)", fc.connects)
	}
	if s := sched(t, pool, a); s == nil || !s.Enabled || s.ConsecutiveFailures != 0 {
		t.Fatalf("a skipped tick must reschedule as a success: %+v", s)
	}
}
