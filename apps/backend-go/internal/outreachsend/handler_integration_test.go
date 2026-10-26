//go:build integration

package outreachsend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachtoken"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// ---- fakes -----------------------------------------------------------------

type call struct {
	cfg      MailConfig
	from, to string
	msg      []byte
}

type fakeMail struct {
	mu      sync.Mutex
	calls   []call
	err     error
	started chan struct{} // closed on first Send when non-nil
	block   chan struct{} // Send waits for close when non-nil
	once    sync.Once
}

var msgIDRe = regexp.MustCompile(`(?m)^Message-ID: (<[^>]+>)`)

func (f *fakeMail) Send(ctx context.Context, cfg MailConfig, from, to string, msg []byte) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{cfg, from, to, msg})
	f.mu.Unlock()
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.err != nil {
		return "", f.err
	}
	if m := msgIDRe.FindSubmatch(msg); m != nil {
		return string(m[1]), nil
	}
	return "", nil
}

func (f *fakeMail) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type memStore struct {
	app map[string]string
	ws  map[string]string
}

func (s *memStore) AppSetting(_ context.Context, k string) (string, error) { return s.app[k], nil }
func (s *memStore) WorkspaceSetting(_ context.Context, ws, k string) (string, bool, error) {
	v, ok := s.ws[ws+"/"+k]
	return v, ok, nil
}

type fakeCaps struct {
	mu                     sync.Mutex
	refuse                 bool
	reserved, settled, rel int
	lastTrigger            string
}

func (c *fakeCaps) TryReserve(_ context.Context, _ pgx.Tx, _, trig string, _ float64, _ string) (Reservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastTrigger = trig
	if c.refuse {
		return Reservation{}, nil
	}
	c.reserved++
	return Reservation{OK: true, Ref: 1}, nil
}
func (c *fakeCaps) Settle(context.Context, pgx.Tx, Reservation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settled++
	return nil
}
func (c *fakeCaps) Release(context.Context, pgx.Tx, Reservation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rel++
	return nil
}

// ---- harness ---------------------------------------------------------------

var masterKey = []byte("QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8=")

var fixedNow = time.Date(2026, 8, 29, 10, 40, 0, 0, time.UTC)

type env struct {
	pool *pgxpool.Pool
	ws   string
	h    *Handler
	mail *fakeMail
	st   *memStore
	now  time.Time
}

func newEnv(t *testing.T, ws string) *env {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	e := &env{pool: pool, ws: ws, mail: &fakeMail{}, now: fixedNow, st: &memStore{app: map[string]string{}, ws: map[string]string{}}}
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "outreach-send-test", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.setSMTP(map[string]string{"SMTP_HOST": "127.0.0.1", "SMTP_PORT": "2525", "SMTP_EMAIL": "sender@contract.example.com",
		"SMTP_PASSWORD": "fake-password-not-real", "SMTP_FROM_NAME": "Contract Sender", "SMTP_USE_TLS": "0", "SMTP_MAX_PER_HOUR": "50"})
	e.st.app["OUTREACH_FOOTER"] = "Contract Co, 1 Example Street"
	e.h = &Handler{
		Pool: pool, Secrets: secrets.NewResolver(envl, e.st, func(string) string { return "" }),
		Signer: outreachtoken.NewSigner(masterKey, "https://app.example.com", 90, func() time.Time { return fixedNow }),
		Mailer: e.mail, Automations: true,
		Thresholds: outreachstore.Thresholds{MinSends: 20, BounceRate: 0.05, ComplaintRate: 0.003},
		Now:        func() time.Time { return e.now },
	}
	e.clean(t)
	t.Cleanup(func() { e.clean(t) })
	return e
}

func (e *env) setSMTP(kv map[string]string) {
	for k := range e.st.app {
		if strings.HasPrefix(k, "SMTP_") {
			delete(e.st.app, k)
		}
	}
	for k, v := range kv {
		if v != "" {
			e.st.app[k] = v
		}
	}
}

func (e *env) clean(t *testing.T) {
	t.Helper()
	_ = tenancy.InWorkspace(context.Background(), e.pool, e.ws, func(tx pgx.Tx) error {
		for _, q := range []string{"outreach_sends", "outreach_enrollments", "outreach_sequences", "outreach_suppressions", "credit_ledger_entries", "workspace_credits"} {
			_, _ = tx.Exec(context.Background(), "DELETE FROM "+q+" WHERE workspace_id=$1", e.ws)
		}
		return nil
	})
}

func (e *env) tx(t *testing.T, fn func(tx pgx.Tx)) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), e.pool, e.ws, func(tx pgx.Tx) error { fn(tx); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	e.tx(t, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	})
}

var defSteps = `[{"step_number":0,"subject":"Hi {{name}}","body_html":"<p>Hello {{name}} at {{company}}</p>","delay_hours":0},` +
	`{"step_number":1,"subject":"Bump {{name}}","body_html":"<p>Bump</p>","delay_hours":24},` +
	`{"step_number":2,"subject":"Last","body_html":"<p>Last</p>","delay_hours":48}]`

func (e *env) seedBasic(t *testing.T) {
	t.Helper()
	e.exec(t, `INSERT INTO outreach_sequences (id,workspace_id,name,description,steps,status,daily_limit,send_window_start,send_window_end,send_window_tz,consent_basis)
	  VALUES ('seq-1',$1,'Seq','',$2::json,'active',50,0,24,'UTC','legit')`, e.ws, defSteps)
	e.exec(t, `INSERT INTO outreach_enrollments (id,workspace_id,sequence_id,lead_id,to_email_snapshot,current_step,status,next_send_at,sent_count,soft_bounce_count,error)
	  VALUES (1,$1,'seq-1',1,'ann@example.com',0,'pending',$2,0,0,'')`, e.ws, fixedNow)
}

func (e *env) payload(over map[string]any) *queue.Claim {
	p := map[string]any{"workspace_id": e.ws, "sequence_id": "seq-1", "enrollment_id": 1, "lead_id": 1, "step_number": 0,
		"to_email": "ann@example.com", "idempotency_key": e.idem(1, 0),
		"variables": map[string]any{"name": "Ann", "company": "Acme", "email": "ann@example.com"}}
	for k, v := range over {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	b, _ := json.Marshal(p)
	return &queue.Claim{ID: 1, Type: "send", WorkspaceID: e.ws, Payload: b}
}

func (e *env) idem(enr, step int) string { return SequenceStepIdem(e.ws, "seq-1", enr, step) }

func (e *env) send(t *testing.T, idem string) *outreachstore.Send {
	t.Helper()
	var s *outreachstore.Send
	e.tx(t, func(tx pgx.Tx) {
		var err error
		s, err = outreachstore.GetSendByIdem(context.Background(), tx, e.ws, idem)
		if err != nil {
			t.Fatal(err)
		}
	})
	return s
}

func (e *env) enr(t *testing.T, id int) *outreachstore.Enrollment {
	t.Helper()
	var r *outreachstore.Enrollment
	e.tx(t, func(tx pgx.Tx) { r, _ = outreachstore.GetEnrollment(context.Background(), tx, id) })
	return r
}

func uniqWS(t *testing.T) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name()))
	return fmt.Sprintf("p09t16-%08x-%d", h.Sum32(), os.Getpid())
}

// ---- send_flow vectors ------------------------------------------------------

type flowSetup struct {
	AutomationsEnabled bool             `json:"automations_enabled"`
	Enrollments        []map[string]any `json:"enrollments"`
	Footer             string           `json:"footer"`
	Master             string           `json:"master"`
	Now                string           `json:"now"`
	PublicBaseURL      string           `json:"public_base_url"`
	Sends              []map[string]any `json:"sends"`
	Sequences          []map[string]any `json:"sequences"`
	SMTPCfg            map[string]any   `json:"smtp_cfg"`
	SMTPScript         map[string]any   `json:"smtp_script"`
	Suppressions       []map[string]any `json:"suppressions"`
}

type flowScenario struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
	Raised  *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"raised"`
	Setup flowSetup `json:"setup"`
	SMTP  struct {
		Commands []string `json:"commands"`
		Data     *string  `json:"data"`
	} `json:"smtp"`
	State struct {
		Enrollments  []map[string]any `json:"enrollments"`
		Sends        []map[string]any `json:"sends"`
		Sequences    []map[string]any `json:"sequences"`
		Suppressions []map[string]any `json:"suppressions"`
	} `json:"state"`
}

func str(m map[string]any, k, def string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return def
}
func intv(m map[string]any, k string, def int) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return def
}
func tsv(t *testing.T, m map[string]any, k string) *time.Time {
	v, ok := m[k].(string)
	if !ok || v == "" {
		return nil
	}
	p, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

func (e *env) seedFlow(t *testing.T, s flowSetup, now time.Time) {
	t.Helper()
	for _, q := range s.Sequences {
		steps, _ := json.Marshal(q["steps"])
		var win, wend, lim, tz any = 0, 24, 50, "UTC"
		if v, ok := q["send_window_start"]; ok {
			win = v
		}
		if v, ok := q["send_window_end"]; ok {
			wend = v
		}
		if v, ok := q["daily_limit"]; ok {
			lim = v
		}
		if v, ok := q["send_window_tz"]; ok {
			tz = v
		}
		e.exec(t, `INSERT INTO outreach_sequences (id,workspace_id,name,description,steps,status,daily_limit,send_window_start,send_window_end,send_window_tz,consent_basis,bounce_count,complaint_count,auto_paused)
		  VALUES ($1,$2,'Seq','',$3::json,$4,$5,$6,$7,$8,'legit',$9,$10,$11)`, q["id"], e.ws, string(steps), str(q, "status", "active"), lim, win, wend, tz,
			intv(q, "bounce_count", 0), intv(q, "complaint_count", 0), q["auto_paused"] == true)
	}
	for _, r := range s.Enrollments {
		next := now
		if p := tsv(t, r, "next_send_at"); p != nil {
			next = *p
		}
		id := intv(r, "id", 1)
		e.exec(t, `INSERT INTO outreach_enrollments (id,workspace_id,sequence_id,lead_id,to_email_snapshot,current_step,status,next_send_at,sent_count,soft_bounce_count,error)
		  VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, e.ws, r["sequence_id"], intv(r, "lead_id", id), r["to_email"], intv(r, "current_step", 0),
			str(r, "status", "pending"), next, intv(r, "sent_count", 0), intv(r, "soft_bounce_count", 0), str(r, "error", ""))
	}
	for _, r := range s.Sends {
		var enr, lead any
		if v, ok := r["enrollment_id"]; ok {
			enr = int(v.(float64))
		}
		if v, ok := r["lead_id"]; ok {
			lead = int(v.(float64))
		}
		var seq, skip any
		if v, ok := r["sequence_id"]; ok {
			seq = v
		}
		if v, ok := r["skip_reason"]; ok {
			skip = v
		}
		e.exec(t, `INSERT INTO outreach_sends (workspace_id,sequence_id,enrollment_id,lead_id,step_number,to_email,subject,status,skip_reason,message_id,idempotency_key,charged_usd,error,sent_at)
		  VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,0,$12,$13)`, e.ws, seq, enr, lead, intv(r, "step_number", 0), r["to_email"], str(r, "subject", ""),
			str(r, "status", "sent"), skip, str(r, "message_id", ""), r["idempotency_key"], str(r, "error", ""), tsv(t, r, "sent_at"))
	}
	for _, r := range s.Suppressions {
		e.exec(t, `INSERT INTO outreach_suppressions (workspace_id,email,reason,source,locked) VALUES ($1,$2,$3,$4,$5)`,
			e.ws, r["email"], str(r, "reason", "manual"), str(r, "source", ""), r["locked"] == true)
	}
}

// compareMessage compares what a receiver sees: decoded headers and decoded part
// bodies. Transfer encoding (7bit vs quoted-printable), header folding, boundary
// and Message-ID differ by design (T9 README deviations).
func compareMessage(t *testing.T, got, want []byte, c call) {
	t.Helper()
	parse := func(raw []byte) (map[string]string, []string) {
		m, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		dec := new(mime.WordDecoder)
		h := map[string]string{}
		for _, k := range []string{"From", "To", "Subject", "List-Unsubscribe", "List-Unsubscribe-Post"} {
			v, _ := dec.DecodeHeader(m.Header.Get(k))
			h[k] = strings.TrimSpace(v)
		}
		if a, err := mail.ParseAddress(h["From"]); err == nil {
			h["From"] = a.Name + " <" + a.Address + ">"
		}
		if m.Header.Get("Message-ID") == "" {
			h["Message-ID"] = "MISSING"
		}
		_, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
		mr := multipart.NewReader(m.Body, params["boundary"])
		var parts []string
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			if strings.EqualFold(p.Header.Get("Content-Transfer-Encoding"), "base64") {
				b, _ = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(b)), ""))
			}
			ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			parts = append(parts, ct+"\n"+strings.ReplaceAll(string(b), "\r\n", "\n"))
		}
		return h, parts
	}
	gh, gp := parse(got)
	wh, wp := parse(want)
	if fmt.Sprint(gh) != fmt.Sprint(wh) {
		t.Errorf("headers differ\n got: %v\nwant: %v", gh, wh)
	}
	if fmt.Sprintf("%q", gp) != fmt.Sprintf("%q", wp) {
		t.Errorf("parts differ\n got: %q\nwant: %q", gp, wp)
	}
	if c.from != c.cfg.Email {
		t.Errorf("envelope from %q", c.from)
	}
}

func scriptedErr(script map[string]any, useTLS bool) error {
	if len(script) == 0 && useTLS {
		return errors.New("STARTTLS extension not supported by server.")
	}
	if r, ok := script["replies"].(map[string]any); ok {
		for _, v := range r {
			var code int
			var msg string
			_, _ = fmt.Sscanf(v.(string), "%d %s", &code, &msg)
			// a protocol reply, as smtpclient surfaces it (textproto.Error)
			return &textproto.Error{Code: code, Msg: strings.TrimSpace(v.(string)[4:])}
		}
	}
	if g, ok := script["greeting"].(string); ok {
		return errors.New(g)
	}
	if _, ok := script["ehlo_extensions"]; ok {
		return errors.New("smtp: server does not support the required extension")
	}
	if _, ok := script["close_after"]; ok {
		return errors.New("smtp: connection unexpectedly closed")
	}
	return nil
}

// Error text differs from Python's str(exc) by design (README T26): outcomes are
// compared by status and hard-bounce classification, never by text.
func TestSendFlowMatchesPythonTranscripts(t *testing.T) {
	raw, err := os.ReadFile("testdata/send_flow.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Scenarios []flowScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Scenarios) != 66 {
		t.Fatalf("scenarios=%d", len(d.Scenarios))
	}
	for _, sc := range d.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			var pl map[string]any
			_ = json.Unmarshal(sc.Payload, &pl)
			ws := pl["workspace_id"].(string)
			e := newEnv(t, ws)
			now, _ := time.Parse(time.RFC3339Nano, sc.Setup.Now)
			e.now = now
			e.h.Automations = sc.Setup.AutomationsEnabled
			e.h.Signer = outreachtoken.NewSigner([]byte(sc.Setup.Master), sc.Setup.PublicBaseURL, 90, func() time.Time { return fixedNow })
			e.st.app["OUTREACH_FOOTER"] = sc.Setup.Footer
			if sc.Setup.SMTPCfg == nil {
				e.setSMTP(nil)
			} else {
				c := sc.Setup.SMTPCfg
				tls := "0"
				if c["use_tls"] == true {
					tls = "1"
				}
				e.setSMTP(map[string]string{"SMTP_HOST": "127.0.0.1", "SMTP_PORT": "2525", "SMTP_EMAIL": str(c, "email", ""),
					"SMTP_PASSWORD": str(c, "password", ""), "SMTP_FROM_NAME": str(c, "from_name", ""), "SMTP_USE_TLS": tls,
					"SMTP_MAX_PER_HOUR": fmt.Sprint(intv(c, "max_per_hour", 50))})
			}
			e.mail.err = scriptedErr(sc.Setup.SMTPScript, sc.Setup.SMTPCfg != nil && sc.Setup.SMTPCfg["use_tls"] == true)
			e.seedFlow(t, sc.Setup, now)

			herr := e.h.Handle(context.Background(), &queue.Claim{ID: 1, Type: "send", WorkspaceID: ws, Payload: sc.Payload})
			if (sc.Raised != nil) != (herr != nil) {
				t.Fatalf("raised=%v err=%v", sc.Raised, herr)
			}
			if sc.Raised != nil && !strings.Contains(herr.Error(), "outreach send transient failure") {
				t.Errorf("error %q lacks transient marker", herr)
			}
			if sc.SMTP.Data != nil {
				if e.mail.n() != 1 {
					t.Fatalf("mailer calls=%d", e.mail.n())
				}
				compareMessage(t, e.mail.calls[0].msg, []byte(*sc.SMTP.Data), e.mail.calls[0])
			} else if len(sc.SMTP.Commands) == 0 && len(sc.Setup.SMTPScript) == 0 && e.mail.n() != 0 {
				t.Errorf("mailer called %d times, Python never dialed", e.mail.n())
			}
			e.compareState(t, sc, now)
		})
	}
}

func (e *env) compareState(t *testing.T, sc flowScenario, now time.Time) {
	t.Helper()
	ctx := context.Background()
	e.tx(t, func(tx pgx.Tx) {
		// sends, matched by idempotency key
		rows, err := tx.Query(ctx, `SELECT sequence_id,enrollment_id,lead_id,step_number,to_email,subject,status,skip_reason,message_id,idempotency_key,charged_usd,error,sent_at,bounced_at
		  FROM outreach_sends WHERE workspace_id=$1 AND idempotency_key NOT LIKE 'fill:%' ORDER BY idempotency_key`, e.ws)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]map[string]any{}
		for rows.Next() {
			var seq, skip, mid, errT, subj *string
			var enr, lead, step *int32
			var to, st, idem string
			var ch *float64
			var sent, bnc *time.Time
			if err := rows.Scan(&seq, &enr, &lead, &step, &to, &subj, &st, &skip, &mid, &idem, &ch, &errT, &sent, &bnc); err != nil {
				t.Fatal(err)
			}
			got[idem] = map[string]any{"sequence_id": seq, "enrollment_id": enr, "lead_id": lead, "step_number": step, "to_email": to, "subject": subj,
				"status": st, "skip_reason": skip, "message_id": mid, "charged_usd": ch, "error": errT, "sent_at": sent, "bounced_at": bnc}
		}
		rows.Close()
		if len(got) != len(sc.State.Sends) {
			t.Errorf("sends: got %d rows want %d", len(got), len(sc.State.Sends))
		}
		for _, w := range sc.State.Sends {
			idem := w["idempotency_key"].(string)
			g, ok := got[idem]
			if !ok {
				t.Errorf("send %s missing", idem)
				continue
			}
			sp := func(k string) string {
				if p, _ := g[k].(*string); p != nil {
					return *p
				}
				return ""
			}
			ip := func(k string) any {
				if p, _ := g[k].(*int32); p != nil {
					return float64(*p)
				}
				return nil
			}
			tp := func(k string) *time.Time { p, _ := g[k].(*time.Time); return p }
			chk := func(name string, a, b any) {
				if fmt.Sprint(a) != fmt.Sprint(b) {
					t.Errorf("send %s %s: got %v want %v", idem, name, a, b)
				}
			}
			chk("status", g["status"], w["status"])
			chk("skip_reason", sp("skip_reason"), str(w, "skip_reason", ""))
			chk("to_email", g["to_email"], w["to_email"])
			chk("subject", sp("subject"), str(w, "subject", ""))
			chk("sequence_id", sp("sequence_id"), str(w, "sequence_id", ""))
			chk("enrollment_id", ip("enrollment_id"), w["enrollment_id"])
			chk("lead_id", ip("lead_id"), w["lead_id"])
			chk("step_number", ip("step_number"), w["step_number"])
			if (str(w, "message_id", "") == "") != (sp("message_id") == "") {
				t.Errorf("send %s message_id presence: got %q want %q", idem, sp("message_id"), w["message_id"])
			}
			if (str(w, "error", "") == "") != (sp("error") == "") {
				t.Errorf("send %s error presence: got %q want %q", idem, sp("error"), w["error"])
			}
			if c, _ := g["charged_usd"].(*float64); c != nil && *c != w["charged_usd"].(float64) {
				t.Errorf("send %s charged %v want %v", idem, *c, w["charged_usd"])
			}
			for _, k := range []string{"sent_at", "bounced_at"} {
				want := tsv(t, w, k)
				if g2 := tp(k); (g2 == nil) != (want == nil) || (g2 != nil && !g2.Equal(*want)) {
					t.Errorf("send %s %s: got %v want %v", idem, k, g2, want)
				}
			}
		}
		// enrollments
		for _, w := range sc.State.Enrollments {
			en, err := outreachstore.GetEnrollment(ctx, tx, intv(w, "id", 0))
			if err != nil || en == nil {
				t.Fatalf("enrollment: %v %v", en, err)
			}
			chk := func(name string, a, b any) {
				if fmt.Sprint(a) != fmt.Sprint(b) {
					t.Errorf("enrollment %s: got %v want %v", name, a, b)
				}
			}
			chk("status", en.Status, w["status"])
			chk("current_step", en.CurrentStep, intv(w, "current_step", 0))
			chk("sent_count", en.SentCount, intv(w, "sent_count", 0))
			chk("soft_bounce_count", en.SoftBounceCount, intv(w, "soft_bounce_count", 0))
			if (str(w, "error", "") == "") != (en.Error == "") {
				t.Errorf("enrollment error presence: got %q want %q", en.Error, w["error"])
			}
			// last_sent_at is stamped with the database clock (AdvanceEnrollment has no
			// clock parameter): check presence and recency instead of the fixed instant.
			if want := tsv(t, w, "last_sent_at"); (en.LastSentAt == nil) != (want == nil) || (want != nil && time.Since(*en.LastSentAt) > time.Minute) {
				t.Errorf("enrollment last_sent_at: got %v want set=%v", en.LastSentAt, want != nil)
			}
			for k, got := range map[string]*time.Time{"next_send_at": en.NextSendAt} {
				want := tsv(t, w, k)
				if (got == nil) != (want == nil) || (got != nil && !got.Equal(*want)) {
					t.Errorf("enrollment %s: got %v want %v", k, got, want)
				}
			}
		}
		// sequences (breaker counters)
		for _, w := range sc.State.Sequences {
			var st *string
			var bc, cc int32
			var ap bool
			if err := tx.QueryRow(ctx, `SELECT status, coalesce(bounce_count,0), coalesce(complaint_count,0), coalesce(auto_paused,false) FROM outreach_sequences WHERE workspace_id=$1 AND id=$2`,
				e.ws, w["id"]).Scan(&st, &bc, &cc, &ap); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(*st, bc, cc, ap) != fmt.Sprint(w["status"], int32(intv(w, "bounce_count", 0)), int32(intv(w, "complaint_count", 0)), w["auto_paused"]) {
				t.Errorf("sequence %v: got %v %d %d %v want %v", w["id"], *st, bc, cc, ap, w)
			}
		}
		// suppressions
		rows, _ = tx.Query(ctx, `SELECT email, reason, source, locked FROM outreach_suppressions WHERE workspace_id=$1 ORDER BY id`, e.ws)
		var gs []string
		for rows.Next() {
			var em, re, so string
			var lk bool
			_ = rows.Scan(&em, &re, &so, &lk)
			gs = append(gs, fmt.Sprint(em, "|", re, "|", so, "|", lk))
		}
		rows.Close()
		var ws []string
		for _, w := range sc.State.Suppressions {
			ws = append(ws, fmt.Sprint(w["email"], "|", w["reason"], "|", w["source"], "|", w["locked"] == true))
		}
		sort.Strings(gs)
		sort.Strings(ws)
		if strings.Join(gs, ";") != strings.Join(ws, ";") {
			t.Errorf("suppressions got %v want %v", gs, ws)
		}
	})
}

// ---- behaviour tests ---------------------------------------------------------

func TestReplayTerminalNoSend(t *testing.T) {
	for _, st := range []string{"sent", "failed", "skipped", "bounced"} {
		t.Run(st, func(t *testing.T) {
			e := newEnv(t, uniqWS(t))
			e.seedBasic(t)
			e.exec(t, `INSERT INTO outreach_sends (workspace_id,sequence_id,enrollment_id,step_number,to_email,status,idempotency_key,message_id) VALUES ($1,'seq-1',1,0,'ann@example.com',$2,$3,'m1')`, e.ws, st, e.idem(1, 0))
			if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
				t.Fatal(err)
			}
			if e.mail.n() != 0 || e.send(t, e.idem(1, 0)).Status != st {
				t.Errorf("%s: terminal row touched or sent", st)
			}
		})
	}
}

func TestReplayInFlightSkipsNeverSends(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	e.exec(t, `INSERT INTO outreach_sends (workspace_id,sequence_id,enrollment_id,step_number,to_email,status,idempotency_key) VALUES ($1,'seq-1',1,0,'ann@example.com','in_flight',$2)`, e.ws, e.idem(1, 0))
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	s := e.send(t, e.idem(1, 0))
	if e.mail.n() != 0 || s.Status != "skipped" || s.SkipReason != "replay" {
		t.Fatalf("calls=%d row=%+v", e.mail.n(), s)
	}
}

func TestInFlightCommittedBeforeSMTP(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	e.mail.started, e.mail.block = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- e.h.Handle(context.Background(), e.payload(nil)) }()
	select {
	case <-e.mail.started:
	case <-time.After(20 * time.Second):
		t.Fatal("mailer never reached")
	}
	if s := e.send(t, e.idem(1, 0)); s == nil || s.Status != "in_flight" {
		t.Fatalf("second connection sees %+v, want committed in_flight", s)
	}
	// TestNoTransactionHeldDuringSMTP: no open tx on any app connection while blocked.
	var open int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE usename=current_user AND pid<>pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("%d transactions held open across the SMTP call", open)
	}
	close(e.mail.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := e.send(t, e.idem(1, 0)); s.Status != "sent" || s.MessageID == "" {
		t.Fatalf("final %+v", s)
	}
}

func TestConcurrentSameIdemSendsOnce(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	var wg sync.WaitGroup
	var fails atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
				fails.Add(1)
				t.Errorf("handle: %v", err)
			}
		}()
	}
	wg.Wait()
	if e.mail.n() != 1 {
		t.Fatalf("sends=%d, want exactly 1", e.mail.n())
	}
	var n int
	e.tx(t, func(tx pgx.Tx) {
		_ = tx.QueryRow(context.Background(), `SELECT count(*) FROM outreach_sends WHERE workspace_id=$1`, e.ws).Scan(&n)
	})
	if n != 1 {
		t.Fatalf("rows=%d", n)
	}
	if s := e.send(t, e.idem(1, 0)); s.Status != "sent" {
		t.Fatalf("status %q (a replay loser must never clobber the winner)", s.Status)
	}
}

func TestSuppressedRechecked(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	e.exec(t, `INSERT INTO outreach_suppressions (workspace_id,email,reason,source,locked) VALUES ($1,'ann@example.com','unsubscribe','x',true)`, e.ws)
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	s := e.send(t, e.idem(1, 0))
	if e.mail.n() != 0 || s.Status != "skipped" || s.SkipReason != "suppressed" || e.enr(t, 1).Status != "suppressed" {
		t.Fatalf("calls=%d row=%+v enr=%s", e.mail.n(), s, e.enr(t, 1).Status)
	}
}

func TestWindowReschedules(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	e.exec(t, `UPDATE outreach_sequences SET send_window_start=12, send_window_end=18 WHERE workspace_id=$1`, e.ws)
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	en := e.enr(t, 1)
	if e.mail.n() != 0 || e.send(t, e.idem(1, 0)) != nil || en.Status != "scheduled" || !en.NextSendAt.Equal(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("calls=%d enr=%+v", e.mail.n(), en)
	}
}

func TestHourlyAndDailyCaps(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	ins := func(i int, at time.Time) {
		e.exec(t, `INSERT INTO outreach_sends (workspace_id,sequence_id,step_number,to_email,status,idempotency_key,sent_at) VALUES ($1,'seq-1',0,$2,'sent',$3,$4)`,
			e.ws, fmt.Sprintf("z%d@example.com", i), fmt.Sprintf("cap-%d", i), at)
	}
	// hourly: max_per_hour=2, two sends 10 minutes ago -> +15 min
	e.setSMTP(map[string]string{"SMTP_HOST": "127.0.0.1", "SMTP_EMAIL": "sender@contract.example.com", "SMTP_PASSWORD": "p", "SMTP_USE_TLS": "0", "SMTP_MAX_PER_HOUR": "2"})
	ins(1, fixedNow.Add(-10*time.Minute))
	ins(2, fixedNow.Add(-10*time.Minute))
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	if en := e.enr(t, 1); e.mail.n() != 0 || !en.NextSendAt.Equal(fixedNow.Add(15*time.Minute)) {
		t.Fatalf("hourly: calls=%d next=%v", e.mail.n(), en.NextSendAt)
	}
	// daily: hourly ok (sends 5h ago), daily_limit=2 -> +1h
	e.exec(t, `UPDATE outreach_sends SET sent_at=$2 WHERE workspace_id=$1`, e.ws, fixedNow.Add(-5*time.Hour))
	e.exec(t, `UPDATE outreach_sequences SET daily_limit=2 WHERE workspace_id=$1`, e.ws)
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	if en := e.enr(t, 1); e.mail.n() != 0 || !en.NextSendAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("daily: calls=%d next=%v", e.mail.n(), en.NextSendAt)
	}
}

func fundWS(t *testing.T, e *env, usd float64) {
	t.Helper()
	e.tx(t, func(tx pgx.Tx) {
		if _, err := billing.Credit(context.Background(), tx, e.ws, usd, "test", "fund:"+e.ws); err != nil {
			t.Fatal(err)
		}
	})
}

func balance(t *testing.T, e *env) float64 {
	var b float64
	e.tx(t, func(tx pgx.Tx) { b, _ = billing.Balance(context.Background(), tx, e.ws) })
	return b
}

func TestDebitOnSuccessOnly(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	caps := &fakeCaps{}
	e.h.Caps, e.h.SendCostUSD, e.h.BillingEnabled = caps, 0.05, true
	fundWS(t, e, 1)
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	s := e.send(t, e.idem(1, 0))
	if s.Status != "sent" || s.ChargedUSD != 0.05 || balance(t, e) != 0.95 || caps.reserved != 1 || caps.settled != 1 || caps.rel != 0 {
		t.Fatalf("row=%+v bal=%v caps=%+v", s, balance(t, e), caps)
	}
	if caps.lastTrigger != "seq-1" {
		t.Errorf("trigger %q", caps.lastTrigger)
	}
}

func TestDebitIdempotentOnRetry(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	e.h.Caps, e.h.SendCostUSD, e.h.BillingEnabled = &fakeCaps{}, 0.05, true
	fundWS(t, e, 1)
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	// the send row vanishes (crash before finalize in another life); the retry's
	// debit finds run:<idem> already charged and charges 0
	e.exec(t, `DELETE FROM outreach_sends WHERE workspace_id=$1`, e.ws)
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	if s := e.send(t, e.idem(1, 0)); s.Status != "sent" || s.ChargedUSD != 0 || balance(t, e) != 0.95 {
		t.Fatalf("row=%+v bal=%v", s, balance(t, e))
	}
}

func TestTransientFailureNeverCharged(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	caps := &fakeCaps{}
	e.h.Caps, e.h.SendCostUSD, e.h.BillingEnabled = caps, 0.05, true
	fundWS(t, e, 1)
	e.mail.err = errors.New("451 4.3.0 try later")
	if err := e.h.Handle(context.Background(), e.payload(nil)); err == nil {
		t.Fatal("transient failure must return an error for the queue retry")
	}
	if s := e.send(t, e.idem(1, 0)); s.Status != "failed" || s.ChargedUSD != 0 || balance(t, e) != 1 {
		t.Fatalf("row=%+v bal=%v", s, balance(t, e))
	}
	// the queue retry short-circuits: terminal row, no second send
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil || e.mail.n() != 1 {
		t.Fatalf("retry err=%v calls=%d", err, e.mail.n())
	}
}

func TestReservationReleasedOnEveryNonSuccess(t *testing.T) {
	cases := map[string]func(e *env){
		"hard_bounce": func(e *env) { e.mail.err = errors.New("550 5.1.1 user unknown") },
		"transient":   func(e *env) { e.mail.err = errors.New("451 4.3.0 try later") },
		"no_footer":   func(e *env) { delete(e.st.app, "OUTREACH_FOOTER") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, uniqWS(t))
			e.seedBasic(t)
			caps := &fakeCaps{}
			e.h.Caps, e.h.SendCostUSD = caps, 0.05
			setup(e)
			_ = e.h.Handle(context.Background(), e.payload(nil))
			if caps.reserved != 1 || caps.rel != 1 || caps.settled != 0 {
				t.Fatalf("caps=%+v", caps)
			}
		})
	}
	t.Run("cap_refused", func(t *testing.T) {
		e := newEnv(t, uniqWS(t))
		e.seedBasic(t)
		e.h.Caps, e.h.SendCostUSD = &fakeCaps{refuse: true}, 0.05
		if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
			t.Fatal(err)
		}
		if s := e.send(t, e.idem(1, 0)); s.Status != "skipped" || s.SkipReason != "cap" || e.mail.n() != 0 {
			t.Fatalf("%+v", s)
		}
	})
}

func TestCostWithoutCapsHandsBack(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	e.h.SendCostUSD = 0.05 // Caps nil
	err := e.h.Handle(context.Background(), e.payload(nil))
	if err != nil && !strings.Contains(err.Error(), "handed back") {
		t.Fatalf("err %v", err)
	}
	if e.mail.n() != 0 || e.send(t, e.idem(1, 0)) != nil {
		t.Fatal("unmetered paid send must never happen or leave rows")
	}
}

func TestMasterSwitchOff(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	e.h.Automations = false
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil || e.mail.n() != 0 || e.send(t, e.idem(1, 0)) != nil {
		t.Fatalf("err=%v calls=%d", err, e.mail.n())
	}
}

func TestCancelledContextBeforeDialSendsNothing(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.h.Handle(ctx, e.payload(nil)); err == nil {
		t.Fatal("cancelled context must surface an error")
	}
	if e.mail.n() != 0 {
		t.Fatal("dialed with a cancelled context")
	}
}

func TestTenantIsolation(t *testing.T) {
	a := newEnv(t, uniqWS(t)+"-a")
	b := &env{pool: a.pool, ws: a.ws + "-b", mail: a.mail, st: a.st, now: a.now, h: a.h}
	b.clean(t)
	t.Cleanup(func() { b.clean(t) })
	a.seedBasic(t) // enrollment 1 lives in workspace A only
	// a job for workspace B naming A's enrollment id finds nothing
	cl := b.payload(map[string]any{"sequence_id": "seq-1"})
	if err := a.h.Handle(context.Background(), cl); err != nil {
		t.Fatal(err)
	}
	if s := b.send(t, b.idem(1, 0)); s == nil || s.SkipReason != "lead_not_found" || a.mail.n() != 0 {
		t.Fatalf("cross-tenant enrollment visible: %+v calls=%d", s, a.mail.n())
	}
	// payload and claim workspace must agree
	bad := a.payload(nil)
	bad.WorkspaceID = "someone-else"
	err := a.h.Handle(context.Background(), bad)
	var pe *queue.PermanentError
	if !errors.As(err, &pe) {
		t.Fatalf("workspace mismatch must be permanent, got %v", err)
	}
}

func TestPayloadVariablesKeepIntegers(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	e.exec(t, `UPDATE outreach_sequences SET steps=$2::json WHERE workspace_id=$1`, e.ws, `[{"step_number":0,"subject":"n={{n}}","body_html":"<p>{{n}}</p>","delay_hours":0}]`)
	cl := e.payload(nil)
	cl.Payload = []byte(strings.Replace(string(cl.Payload), `"variables":{`, `"variables":{"n":5,`, 1))
	if err := e.h.Handle(context.Background(), cl); err != nil {
		t.Fatal(err)
	}
	if s := e.send(t, e.idem(1, 0)); s == nil || s.Subject != "n=5" {
		t.Fatalf("%+v", s)
	}
}

func TestSendFlowCompleteWithoutDateHeader(t *testing.T) {
	e := newEnv(t, uniqWS(t))
	e.seedBasic(t)
	if err := e.h.Handle(context.Background(), e.payload(nil)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(e.mail.calls[0].msg), "\r\nDate:") {
		t.Fatal("Python adds no Date header")
	}
}
