package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoschedule"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Config is the slice of the INTENT_POLLER_* / POLLER_*_COST_USD settings the
// core needs. It is a plain struct so tests and the worker build it directly.
type Config struct {
	DefaultInterval        string // INTENT_POLLER_DEFAULT_INTERVAL
	MaxConsecutiveFailures int    // INTENT_POLLER_MAX_CONSECUTIVE_FAILURES
	DailyPollBudget        int    // INTENT_POLLER_DAILY_POLL_BUDGET (0 = unlimited)
	FundingCostUSD         float64
	HiringCostUSD          float64
	FeedCostUSD            float64
	// BillingEnabled gates every per-source debit (BILLING_ENABLED).
	BillingEnabled bool
	// WebsiteTech selects the web_tech detector over jobspy tech (TECH_STACK_WEBSITE_FETCH_ENABLED).
	WebsiteTech bool
}

// ConfigFrom builds a Config from the loaded automation settings.
func ConfigFrom(a config.AutomationsSettings, billingEnabled bool) Config {
	return Config{
		DefaultInterval:        a.PollerDefaultInterval,
		MaxConsecutiveFailures: a.PollerMaxConsecutiveFailures,
		DailyPollBudget:        a.PollerDailyPollBudget,
		FundingCostUSD:         a.PollerFundingCostUSD,
		HiringCostUSD:          a.PollerHiringCostUSD,
		FeedCostUSD:            a.PollerFeedCostUSD,
		BillingEnabled:         billingEnabled,
		WebsiteTech:            a.TechStackWebsiteFetchEnabled,
	}
}

// Watch is the slice of a watch_subscriptions row the scheduling core reads
// and writes. Times are UTC; nil pointers are SQL NULL.
type Watch struct {
	ID, WorkspaceID     string
	Interval            string // "" when NULL (Python falls back to the default)
	Anchor              *time.Time
	Enabled             bool
	NextPollAt          *time.Time
	LastError           *string
	ConsecutiveFailures int
}

func tsz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func fromTsz(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// LoadWatch reads and row-locks one watch inside tenancy.InWorkspace. ok is
// false when the watch is missing in that workspace.
func LoadWatch(ctx context.Context, tx pgx.Tx, id, ws string) (Watch, bool, error) {
	r, err := gen.New(tx).PollLoadWatch(ctx, gen.PollLoadWatchParams{ID: id, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return Watch{}, false, nil
	}
	if err != nil {
		return Watch{}, false, fmt.Errorf("poller: load watch: %w", err)
	}
	w := Watch{ID: r.ID, WorkspaceID: r.WorkspaceID, Enabled: r.Enabled, Anchor: fromTsz(r.ScheduleAnchor),
		NextPollAt: fromTsz(r.NextPollAt), ConsecutiveFailures: int(r.ConsecutiveFailures)}
	if r.PollInterval.Valid {
		w.Interval = r.PollInterval.String
	}
	if r.LastError.Valid {
		s := r.LastError.String
		w.LastError = &s
	}
	return w, true, nil
}

func saveWatch(ctx context.Context, tx pgx.Tx, w Watch) error {
	p := gen.PollSaveWatchHealthParams{ID: w.ID, WorkspaceID: w.WorkspaceID, ConsecutiveFailures: int32(w.ConsecutiveFailures),
		Enabled: w.Enabled, NextPollAt: tsz(w.NextPollAt)}
	if w.LastError != nil {
		p.LastError = pgtype.Text{String: *w.LastError, Valid: true}
	}
	if err := gen.New(tx).PollSaveWatchHealth(ctx, p); err != nil {
		return fmt.Errorf("poller: save watch: %w", err)
	}
	return nil
}

// SourceSet is _source_set: the independent-transaction sources a watch kind
// fans in. Empty or missing signalTypes wants every type. new_tech_adopted has
// two mutually exclusive detectors (jobspy "tech" or website "web_tech"); the
// SEC detectors share one watermark and so one source when both are wanted.
func SourceSet(kind string, signalTypes []string, webTech bool) []string {
	types := map[string]bool{}
	for _, s := range signalTypes {
		types[s] = true
	}
	wants := func(sts ...string) bool {
		if len(types) == 0 {
			return true
		}
		for _, s := range sts {
			if types[s] {
				return true
			}
		}
		return false
	}
	tech := "tech"
	if webTech {
		tech = "web_tech"
	}
	var sec []string
	switch {
	case wants("company_funded") && wants("executive_hired"):
		sec = []string{"funding_exec"}
	case wants("company_funded"):
		sec = []string{"funding"}
	case wants("executive_hired"):
		sec = []string{"exec"}
	}
	switch kind {
	case "funding":
		if len(sec) > 0 {
			return sec
		}
		return []string{"funding"}
	case "hiring":
		var out []string
		if wants("hiring_surge") {
			out = append(out, "hiring")
		}
		if wants("new_tech_adopted") {
			out = append(out, tech)
		}
		if len(out) > 0 {
			return out
		}
		return []string{"hiring", tech}
	case "feed":
		return []string{"feed"}
	case "job_change":
		return []string{"job_change"}
	case "account_group":
		return []string{"account_group"}
	case "company":
		out := append([]string(nil), sec...)
		if wants("hiring_surge") {
			out = append(out, "hiring")
		}
		if wants("new_tech_adopted") {
			out = append(out, tech)
		}
		if len(out) > 0 {
			return out
		}
		return []string{"funding_exec", "hiring", tech}
	}
	return []string{}
}

// isoformat renders t like Python's isoformat() for a UTC-aware datetime:
// microseconds only when non-zero, then "+00:00". Fire keys are always UTC
// (audit F1), independent of the Postgres session TimeZone.
func isoformat(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "+00:00"
}

// ScheduledFireKey is scheduled_fire_key with the instant rendered in UTC:
// watch:<id>:<next.isoformat()>.
func ScheduledFireKey(watchID string, next time.Time) string {
	return "watch:" + watchID + ":" + isoformat(next)
}

type pollPayload struct {
	WatchID     string `json:"watch_id"`
	WorkspaceID string `json:"workspace_id"`
	FireKey     string `json:"fire_key"`
	PollNow     *bool  `json:"poll_now,omitempty"`
}

// EnqueuePollIfAbsent is _enqueue_poll_if_absent: single-flight insert of a
// watch_poll job under pg_advisory_xact_lock(hashtext('watch_poll:'||fire_key))
// (the exact text Python locks, so Go and Python serialise on one lock), then
// the insert guarded by uq_jobs_fire_key_active. created is false when an
// active job already holds the key. A manual poll carries poll_now in the same
// insert (no follow-up UPDATE a crash could separate). tx must be inside
// tenancy.InWorkspace; the job rolls back with it.
func EnqueuePollIfAbsent(ctx context.Context, tx pgx.Tx, fireKey, ws, watchID string, next time.Time, pollNow bool, executor string) (bool, error) {
	if executor != "go" {
		return false, fmt.Errorf("poller: executor %q must be go (the only executor since the plan 15 cutover)", executor)
	}
	if fireKey == "" || ws == "" || watchID == "" {
		return false, errors.New("poller: enqueue needs fire key, workspace and watch")
	}
	p := pollPayload{WatchID: watchID, WorkspaceID: ws, FireKey: fireKey}
	if pollNow {
		t := true
		p.PollNow = &t
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return false, fmt.Errorf("poller: encode payload: %w", err)
	}
	q := gen.New(tx)
	if err := q.LockJobFireKey(ctx, "watch_poll:"+fireKey); err != nil {
		return false, fmt.Errorf("poller: lock: %w", err)
	}
	if _, err := q.PollActiveWatchJob(ctx, fireKey); err == nil {
		return false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("poller: look up: %w", err)
	}
	_, err := q.PollInsertWatchJob(ctx, gen.PollInsertWatchJobParams{
		Executor: executor, Payload: bytes.TrimRight(buf.Bytes(), "\n"), WorkspaceID: ws, FireKey: fireKey,
		NextRunAt: pgtype.Timestamptz{Time: next.UTC(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // lost the unique-index race
	}
	if err != nil {
		return false, fmt.Errorf("poller: enqueue: %w", err)
	}
	return true, nil
}

// MirrorSet is mirror_set: upsert the non-RLS watch_schedules row in the
// caller's transaction. A nil next stores NULL (due at the next bootstrap).
func MirrorSet(ctx context.Context, tx pgx.Tx, watchID, ws string, next *time.Time, enabled bool) error {
	err := gen.New(tx).PollMirrorUpsert(ctx, gen.PollMirrorUpsertParams{WatchID: watchID, WorkspaceID: ws, NextPollAt: tsz(next), Enabled: enabled})
	if err != nil {
		return fmt.Errorf("poller: mirror set: %w", err)
	}
	return nil
}

// MirrorDelete is mirror_delete.
func MirrorDelete(ctx context.Context, tx pgx.Tx, watchID string) error {
	if err := gen.New(tx).PollMirrorDelete(ctx, watchID); err != nil {
		return fmt.Errorf("poller: mirror delete: %w", err)
	}
	return nil
}

const maxBackoff = 24 * time.Hour

func (c Config) interval(w Watch) string {
	if w.Interval != "" {
		return w.Interval
	}
	return c.DefaultInterval
}

// NextPollFor is the aligned next poll instant for a watch:
// anchor + n*interval on absolute instants, independent of the Postgres session
// TimeZone (audit F1). A NULL anchor means "now" and Python re-reads the clock,
// so the result is one interval ahead.
func NextPollFor(w Watch, now time.Time, c Config) time.Time {
	if w.Anchor == nil {
		return now.UTC().Add(autoschedule.IntervalDelta(c.interval(w)))
	}
	return autoschedule.NextRunIn(*w.Anchor, c.interval(w), now.UTC(), time.UTC)
}

// RescheduleWatch is _reschedule_watch and persists the result (health columns,
// the job and the mirror all join tx). Success resets consecutive_failures. A
// failure increments it: at MaxConsecutiveFailures the watch is disabled with
// last_error "auto_disabled", next_poll_at NULL, the mirror disabled and
// nothing enqueued; otherwise next = max(aligned, now + min(interval/4 *
// 2^(failures-1), 24h)). A disabled watch never enqueues. It returns the
// updated watch.
func RescheduleWatch(ctx context.Context, tx pgx.Tx, w Watch, failed bool, now time.Time, cfg Config, executor string) (Watch, error) {
	now = now.UTC().Truncate(time.Microsecond)
	interval := cfg.interval(w)
	next := NextPollFor(w, now, cfg)
	if failed {
		w.ConsecutiveFailures++
		if w.ConsecutiveFailures >= cfg.MaxConsecutiveFailures {
			return disable(ctx, tx, w)
		}
		backoff := autoschedule.IntervalDelta(interval) / 4
		for i := 1; i < w.ConsecutiveFailures && backoff < maxBackoff; i++ {
			backoff *= 2
		}
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		if until := now.Add(backoff); until.After(next) {
			next = until
		}
	} else {
		w.ConsecutiveFailures = 0
	}
	w.NextPollAt = &next
	if w.Enabled {
		if _, err := EnqueuePollIfAbsent(ctx, tx, ScheduledFireKey(w.ID, next), w.WorkspaceID, w.ID, next, false, executor); err != nil {
			return w, err
		}
		if err := MirrorSet(ctx, tx, w.ID, w.WorkspaceID, &next, true); err != nil {
			return w, err
		}
	}
	return w, saveWatch(ctx, tx, w)
}

// disable is the auto-disable branch shared with the failure reconcile.
func disable(ctx context.Context, tx pgx.Tx, w Watch) (Watch, error) {
	w.Enabled = false
	le := "auto_disabled"
	w.LastError = &le
	w.NextPollAt = nil
	if err := MirrorSet(ctx, tx, w.ID, w.WorkspaceID, nil, false); err != nil {
		return w, err
	}
	return w, saveWatch(ctx, tx, w)
}
