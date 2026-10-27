//go:build integration

package autoactions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const loWS = "ws-p15-autolegacy"

type loVec struct {
	Name     string          `json:"name"`
	Action   json.RawMessage `json:"action"`
	LeadID   *int64          `json:"lead_id"`
	Lead     map[string]any  `json:"lead"`
	Idem     *string         `json:"idem"`
	Repeat   int             `json:"repeat"`
	Preset   *string         `json:"preset"`
	Outcomes []struct {
		Result *struct {
			Status     string  `json:"status"`
			Summary    string  `json:"summary"`
			Error      *string `json:"error"`
			SkipReason *string `json:"skip_reason"`
		} `json:"result"`
		Raised *string `json:"raised"`
	} `json:"outcomes"`
	Enrollments []struct {
		LeadID        int64  `json:"lead_id"`
		Email         string `json:"to_email_snapshot"`
		ConsentSource string `json:"consent_source"`
		ConsentAtSet  bool   `json:"consent_at_set"`
	} `json:"enrollments"`
	Jobs []struct {
		Executor string         `json:"executor"`
		Payload  map[string]any `json:"payload"`
	} `json:"jobs"`
}

// Deliberate departures from Python (WP-AUTOLEGACY report): a suppressed address
// never gets enrolled or queued by send_email, and a lead-less standalone send is
// keyed by the address instead of the literal "None". Review wave 2a M4: a trigger row without a lead id is
// refused by sequencer (Python enrolled it as lead_id 0, shared by every lead-less row).
var loDeviation = map[string]bool{"send_email_seq_suppressed": true, "send_email_standalone_lead_none": true, "sequencer_lead_none": true}

func loClean(t *testing.T, ctx context.Context, d Deps) {
	t.Helper()
	fn := func() {
		_ = tenancy.InWorkspace(ctx, d.Pool, loWS, func(tx pgx.Tx) error {
			for _, tbl := range []string{"outreach_sends", "outreach_enrollments", "outreach_suppressions", "outreach_sequences"} {
				_, _ = tx.Exec(ctx, `DELETE FROM `+tbl)
			}
			_, err := tx.Exec(ctx, `DELETE FROM jobs WHERE type='send' AND workspace_id=$1`, loWS)
			return err
		})
	}
	fn()
	t.Cleanup(fn)
}

func loSeq(t *testing.T, ctx context.Context, d Deps) string {
	t.Helper()
	var id string
	if err := tenancy.InWorkspace(ctx, d.Pool, loWS, func(tx pgx.Tx) error {
		s, err := outreachstore.CreateSequence(ctx, tx, loWS, outreachstore.NewSeqInput("S"))
		id = s.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func loIn(t *testing.T, ctx context.Context, d Deps, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, d.Pool, loWS, fn); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyOutreachVectors(t *testing.T) {
	ctx := context.Background()
	d := Deps{Pool: testpg.NewAppPool(t, 8)}
	for _, v := range loadVectors[loVec](t, "legacy_outreach_vectors.json") {
		t.Run(v.Name, func(t *testing.T) {
			loClean(t, ctx, d)
			seq := loSeq(t, ctx, d)
			if v.Preset != nil {
				loIn(t, ctx, d, func(tx pgx.Tx) error {
					switch *v.Preset {
					case "enrolled":
						_, _, err := outreachstore.Enroll(ctx, tx, loWS, seq, 7, "a@x.com", "x", time.Time{})
						return err
					case "suppressed":
						_, err := outreachstore.AddSuppression(ctx, tx, loWS, "a@x.com", "manual", "test", false)
						return err
					}
					return fmt.Errorf("unknown preset %q", *v.Preset)
				})
			}
			var action map[string]any
			dec := json.NewDecoder(strings.NewReader(strings.ReplaceAll(string(v.Action), "$SEQ", seq)))
			dec.UseNumber()
			if err := dec.Decode(&action); err != nil {
				t.Fatal(err)
			}
			idem := ""
			if v.Idem != nil {
				idem = *v.Idem
			}
			for i, o := range v.Outcomes {
				got := Execute(ctx, d, loWS, action, "wb", "1", v.LeadID, v.Lead, nil, idem)
				if o.Raised != nil { // Python raised: Go answers a failed result instead
					if got.Status != "failed" {
						t.Fatalf("run %d: want failed for a Python exception, got %+v", i, got)
					}
					continue
				}
				if loDeviation[v.Name] {
					continue
				}
				w := o.Result
				if got.Status != w.Status || got.Summary != w.Summary || got.Error != deref(w.Error) || got.SkipReason != deref(w.SkipReason) {
					t.Fatalf("run %d: got %+v, want %+v", i, got, *w)
				}
			}
			if loDeviation[v.Name] {
				return
			}
			var enr, jobs int
			var firstEnr int32
			loIn(t, ctx, d, func(tx pgx.Tx) error {
				_ = tx.QueryRow(ctx, `SELECT coalesce(min(id),0) FROM outreach_enrollments`).Scan(&firstEnr)
				rows, err := tx.Query(ctx, `SELECT lead_id, to_email_snapshot, consent_source, consent_at IS NOT NULL FROM outreach_enrollments ORDER BY id`)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var lead int32
					var email, src string
					var set bool
					if err := rows.Scan(&lead, &email, &src, &set); err != nil {
						return err
					}
					if enr >= len(v.Enrollments) {
						t.Fatalf("extra enrollment %d %s", lead, email)
					}
					w := v.Enrollments[enr]
					if int64(lead) != w.LeadID || email != w.Email || src != w.ConsentSource || set != w.ConsentAtSet {
						t.Fatalf("enrollment %d: got %d %s %s %v, want %+v", enr, lead, email, src, set, w)
					}
					enr++
				}
				return rows.Err()
			})
			if enr != len(v.Enrollments) {
				t.Fatalf("enrollments: got %d, want %d", enr, len(v.Enrollments))
			}
			loIn(t, ctx, d, func(tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT executor, payload::text FROM jobs WHERE type='send' ORDER BY id`)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var ex, raw string
					if err := rows.Scan(&ex, &raw); err != nil {
						return err
					}
					if jobs >= len(v.Jobs) {
						t.Fatalf("extra job %s", raw)
					}
					w := v.Jobs[jobs]
					var got map[string]any
					g := json.NewDecoder(strings.NewReader(raw))
					g.UseNumber()
					_ = g.Decode(&got)
					delete(got, "fire_key")
					gj, _ := json.Marshal(got)
					wp := map[string]any{}
					for k, x := range w.Payload {
						wp[k] = x
					}
					if _, ok := wp["enrollment_id"].(json.Number); ok || wp["enrollment_id"] != nil {
						// the vector's ids come from a fresh sqlite; ours from a shared sequence
						old := fmt.Sprint(wp["enrollment_id"])
						wp["enrollment_id"] = json.Number(fmt.Sprint(firstEnr))
						wp["idempotency_key"] = strings.Replace(fmt.Sprint(wp["idempotency_key"]), ":"+old+":", fmt.Sprintf(":%d:", firstEnr), 1)
					}
					wj, _ := json.Marshal(wp)
					wantS := strings.ReplaceAll(strings.ReplaceAll(string(wj), "$SEQ", seq), `"ws-1"`, `"`+loWS+`"`)
					if v.Idem == nil || strings.HasPrefix(fmt.Sprint(wp["idempotency_key"]), "seq:") {
						wantS = strings.ReplaceAll(wantS, ":ws-1:", ":"+loWS+":") // keys Go derives; an engine key passes through
					}
					if ex != w.Executor || normJSON(t, string(gj)) != normJSON(t, wantS) {
						t.Fatalf("job %d: executor %s payload %s\nwant %s %s", jobs, ex, gj, w.Executor, wantS)
					}
					jobs++
				}
				return rows.Err()
			})
			if jobs != len(v.Jobs) {
				t.Fatalf("jobs: got %d, want %d", jobs, len(v.Jobs))
			}
		})
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func normJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v) // sorted keys, canonical numbers
	return string(b)
}

// Deviations from Python, pinned explicitly.
func TestLegacyOutreachSuppressedNeverQueued(t *testing.T) {
	ctx := context.Background()
	d := Deps{Pool: testpg.NewAppPool(t, 8)}
	loClean(t, ctx, d)
	seq := loSeq(t, ctx, d)
	loIn(t, ctx, d, func(tx pgx.Tx) error {
		_, err := outreachstore.AddSuppression(ctx, tx, loWS, "a@x.com", "unsubscribe", "t", true)
		return err
	})
	lead := map[string]any{"email": " A@X.com "}
	for name, cfg := range map[string]map[string]any{"standalone": {}, "sequence": {"sequence_id": seq}} {
		got := Execute(ctx, d, loWS, map[string]any{"type": "send_email", "config": cfg}, "wb", "1", ptr(7), lead, nil, "k-"+name)
		if got.Status != "skipped" || got.SkipReason != "suppressed" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	var jobs, enr int
	loIn(t, ctx, d, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type='send' AND workspace_id=$1`, loWS).Scan(&jobs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM outreach_enrollments`).Scan(&enr)
	})
	if jobs != 0 || enr != 0 {
		t.Fatalf("suppressed address left %d jobs, %d enrollments", jobs, enr)
	}
}

func TestLegacyOutreachLeadlessKeysDiffer(t *testing.T) {
	ctx := context.Background()
	d := Deps{Pool: testpg.NewAppPool(t, 8)}
	loClean(t, ctx, d)
	for _, e := range []string{"a@x.com", "b@x.com"} {
		r := Execute(ctx, d, loWS, map[string]any{"type": "send_email", "config": map[string]any{}}, "wb", "1", nil, map[string]any{"email": e}, nil, "")
		if r.Summary != "send enqueued" {
			t.Fatalf("%s: %+v", e, r)
		}
	}
}

func ptr(i int64) *int64 { return &i }

// At-most-once: an in_flight (or any recorded) send row for the key, a pending
// job, and concurrent identical calls each yield exactly one job and never a send.
func TestLegacySendEmailAtMostOnce(t *testing.T) {
	ctx := context.Background()
	d := Deps{Pool: testpg.NewAppPool(t, 16), SendExecutor: func(string) string { return "go" }}
	loClean(t, ctx, d)
	act := map[string]any{"type": "send_email", "config": map[string]any{"subject": "s", "body_html": "<p>{{name}}</p>"}}
	lead := map[string]any{"email": "a@x.com", "name": "<b>A</b>"}

	// a send already in_flight for the key: the action must not queue another
	loIn(t, ctx, d, func(tx pgx.Tx) error {
		_, err := outreachstore.InsertInFlight(ctx, tx, outreachstore.SendIn{WorkspaceID: loWS, ToEmail: "a@x.com", IdempotencyKey: "k-inflight"})
		return err
	})
	if r := Execute(ctx, d, loWS, act, "wb", "1", ptr(7), lead, nil, "k-inflight"); r.Status != "success" || r.Summary != "already queued/sent" {
		t.Fatalf("in_flight replay: %+v", r)
	}

	var wg sync.WaitGroup
	sums := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sums <- Execute(ctx, d, loWS, act, "wb", "1", ptr(7), lead, nil, "k-race").Summary
		}()
	}
	wg.Wait()
	close(sums)
	enq := 0
	for s := range sums {
		if s == "send enqueued" {
			enq++
		}
	}
	var jobs int
	var exec string
	loIn(t, ctx, d, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), coalesce(max(executor),'') FROM jobs WHERE type='send' AND workspace_id=$1`, loWS).Scan(&jobs, &exec)
	})
	if enq != 1 || jobs != 1 || exec != "go" {
		t.Fatalf("concurrent calls: %d reported enqueued, %d jobs, executor %q (want 1, 1, go)", enq, jobs, exec)
	}
}

func loCount(t *testing.T, ctx context.Context, d Deps) (jobs, enr int) {
	t.Helper()
	loIn(t, ctx, d, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type='send' AND workspace_id=$1`, loWS).Scan(&jobs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM outreach_enrollments`).Scan(&enr)
	})
	return jobs, enr
}

// M4: a trigger row without a lead id is never enrolled (it would share enrollment lead_id=0 and send one
// recipient's data to another).
func TestLegacyOutreachRefusesEnrollmentWithoutLeadID(t *testing.T) {
	ctx := context.Background()
	d := Deps{Pool: testpg.NewAppPool(t, 8)}
	loClean(t, ctx, d)
	seq := loSeq(t, ctx, d)
	for _, typ := range []string{"sequencer", "send_email"} {
		for _, e := range []string{"a@x.com", "b@x.com"} {
			act := map[string]any{"type": typ, "config": map[string]any{"sequence_id": seq}}
			r := Execute(ctx, d, loWS, act, "wb", "1", nil, map[string]any{"email": e, "name": e}, nil, "")
			if r.Status != "failed" || !strings.Contains(r.Error, "lead") {
				t.Fatalf("%s %s: %+v, want failed naming the missing lead id", typ, e, r)
			}
		}
	}
	if jobs, enr := loCount(t, ctx, d); jobs != 0 || enr != 0 {
		t.Fatalf("refused rows left %d jobs, %d enrollments", jobs, enr)
	}
}

// M4: an existing enrollment is reused only when the stored email matches the lead's current email.
func TestLegacyOutreachNeverReusesEnrollmentOfAnotherEmail(t *testing.T) {
	ctx := context.Background()
	d := Deps{Pool: testpg.NewAppPool(t, 8)}
	loClean(t, ctx, d)
	seq := loSeq(t, ctx, d)
	loIn(t, ctx, d, func(tx pgx.Tx) error {
		_, _, err := outreachstore.Enroll(ctx, tx, loWS, seq, 7, "a@x.com", "x", time.Time{})
		return err
	})
	for _, typ := range []string{"sequencer", "send_email"} {
		act := map[string]any{"type": typ, "config": map[string]any{"sequence_id": seq}}
		r := Execute(ctx, d, loWS, act, "wb", "1", ptr(7), map[string]any{"email": "b@x.com"}, nil, "")
		if r.Status != "failed" || !strings.Contains(r.Error, "email") {
			t.Fatalf("%s: %+v, want failed (stored email differs)", typ, r)
		}
	}
	if jobs, _ := loCount(t, ctx, d); jobs != 0 {
		t.Fatalf("a mismatched enrollment queued %d sends", jobs)
	}
	// same email (any case) still reuses it
	act := map[string]any{"type": "send_email", "config": map[string]any{"sequence_id": seq}}
	if r := Execute(ctx, d, loWS, act, "wb", "1", ptr(7), map[string]any{"email": "A@X.com"}, nil, ""); r.Status != "success" {
		t.Fatalf("matching email: %+v", r)
	}
}
