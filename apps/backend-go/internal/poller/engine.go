package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	// The web_tech source resolves the "tech_stack" provider through the registry.
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// HandlerDeps are the dependencies of the watch_poll handler. PyDeps carries
// the poller Deps (pool, config, clock, executor routing), the HTTP-source deps
// (egress client, shared host spacer; build ONE and share it, its AppEnv and
// EgressAllowHosts must equal the client's) and the pyworker collectors.
type HandlerDeps struct {
	PyDeps
	// Hook receives every inserted signal in the emitting transaction (nil: none).
	Hook automationhook.Hook
	// Enabled is INTENT_POLLER_ENABLED; PGStore is PG_LEAD_STORE.
	Enabled, PGStore bool
	// Backfill is INTENT_POLLER_BACKFILL.
	Backfill bool
	// Log defaults to slog.Default().
	Log *slog.Logger
	// Fetch is a test seam: when it reports handled the real source fetch is skipped.
	Fetch func(ctx context.Context, src string, w PyWatch, lead int64, backfill bool) (evs []DetectedEvent, patch map[string]any, handled bool, err error)
}

func (d HandlerDeps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// WatchRow is the slice of a watch the poll reads: the routing fields of the
// subscription and its decoded JSON columns.
type WatchRow struct {
	ID, WorkspaceID, Kind, Target string
	ResolvedCIK                   string
	LeadID                        int64 // 0 = not pinned
	SignalTypes                   []string
	Config                        map[string]any
	Cursor                        map[string]any
	Enabled                       bool
}

func decodeObject(raw []byte) map[string]any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}
	}
	v, err := providers.ParseJSON(raw)
	if err != nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func decodeStrings(raw []byte) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	v, err := providers.ParseJSON(raw)
	if err != nil {
		return nil
	}
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func loadWatchRow(ctx context.Context, tx pgx.Tx, id, ws string) (WatchRow, bool, error) {
	r, err := gen.New(tx).PollEngineGet(ctx, gen.PollEngineGetParams{ID: id, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return WatchRow{}, false, nil
	}
	if err != nil {
		return WatchRow{}, false, fmt.Errorf("poller: load watch: %w", err)
	}
	w := WatchRow{ID: r.ID, WorkspaceID: r.WorkspaceID, Kind: r.Kind, Target: r.Target, Enabled: r.Enabled,
		SignalTypes: decodeStrings(r.SignalTypes), Config: decodeObject(r.Config), Cursor: decodeObject(r.Cursor)}
	if r.ResolvedCik.Valid {
		w.ResolvedCIK = r.ResolvedCik.String
	}
	if r.LeadID.Valid {
		w.LeadID = int64(r.LeadID.Int32)
	}
	return w, true, nil
}

// ResolveLeadID is _resolve_lead_id, reading lead rows through the tenant only.
// A pinned lead wins; job_change and account_group route per event; otherwise
// the exact normalised company match with the lowest id, a single fuzzy match,
// "ambiguous_lead" for several fuzzy matches, and (0, "") for none (skip
// silently).
func ResolveLeadID(ctx context.Context, tx pgx.Tx, w WatchRow) (lead int64, errText string, err error) {
	if w.LeadID != 0 {
		return w.LeadID, "", nil
	}
	if w.Kind == "job_change" || w.Kind == "account_group" {
		return 0, "", nil
	}
	target := NormalizeTarget(w.Target)
	if target == "" {
		return 0, "", nil
	}
	rows, err := gen.New(tx).PollEngineLeadCompanies(ctx, w.WorkspaceID)
	if err != nil {
		return 0, "", fmt.Errorf("poller: lead lookup: %w", err)
	}
	var exact int64
	var fuzzy []int64
	for _, r := range rows {
		cn := NormalizeTarget(r.Company)
		if cn == "" {
			continue
		}
		id := int64(r.ID)
		switch {
		case cn == target:
			if exact == 0 || id < exact {
				exact = id
			}
		case containsStr(cn, target) || containsStr(target, cn):
			fuzzy = append(fuzzy, id)
		}
	}
	switch {
	case exact != 0:
		return exact, "", nil
	case len(fuzzy) > 1:
		return 0, "ambiguous_lead", nil
	case len(fuzzy) == 1:
		return fuzzy[0], "", nil
	}
	return 0, "", nil
}

func containsStr(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// EmitCounts is the outcome of EmitEvents.
type EmitCounts struct{ Inserted, Dupe int }

// EmitEvents is _emit_events + SignalStore.add_signal: every event becomes a
// row under its deterministic signal_event_id (ON CONFLICT (id) DO NOTHING), and
// SignalsInserted fires inside tx for the rows actually inserted only. A
// per-event lead (job_change / account_group) overrides the watch-level lead;
// created_at is the event time or now.
func EmitEvents(ctx context.Context, tx pgx.Tx, hook automationhook.Hook, w WatchRow, lead int64, evs []DetectedEvent, now time.Time) (EmitCounts, error) {
	var out EmitCounts
	var rc *string
	if w.ResolvedCIK != "" {
		rc = &w.ResolvedCIK
	}
	stable := StableCompanyID(rc, w.Target)
	q := gen.New(tx)
	var inserted []automationhook.SignalRef
	for _, ev := range evs {
		id := SignalEventID(w.WorkspaceID, w.Kind, stable, ev.SignalType, ev.NaturalEventID)
		l := lead
		if ev.LeadID != nil {
			l = *ev.LeadID
		}
		if l < 0 || l > math.MaxInt32 {
			l = 0
		}
		company := ev.Company
		if company == "" {
			company = w.Target
		}
		created := ev.OccurredAt
		if created == 0 {
			created = float64(now.UnixNano()) / 1e9
		}
		_, err := q.ScanInsertSignal(ctx, gen.ScanInsertSignalParams{
			ID: id, WorkspaceID: w.WorkspaceID, LeadID: int32(l), Company: company, SignalType: ev.SignalType,
			Title: ev.Title, Description: ev.Description, Source: ev.Source, SourceUrl: ev.SourceURL,
			Weight: int32(ev.Weight), CreatedAt: created,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			out.Dupe++
			continue
		}
		if err != nil {
			return out, fmt.Errorf("poller: insert signal: %w", err)
		}
		out.Inserted++
		ref := automationhook.SignalRef{PK: id, Type: ev.SignalType}
		if l != 0 {
			ll := l
			ref.LeadID = &ll
		}
		inserted = append(inserted, ref)
	}
	if hook != nil && len(inserted) > 0 {
		if _, err := hook.SignalsInserted(ctx, tx, w.WorkspaceID, inserted); err != nil {
			return out, fmt.Errorf("poller: signal hook: %w", err)
		}
	}
	return out, nil
}

type pollPayloadIn struct {
	WorkspaceID any `json:"workspace_id"`
	WatchID     any `json:"watch_id"`
	FireKey     any `json:"fire_key"`
	PollNow     any `json:"poll_now"`
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// sourceOutcome is one source's result.
type sourceOutcome struct {
	failed         bool
	inserted, dupe int
}

// Handle is handle_watch_poll. Flag off, no PG store, or a payload without
// workspace_id/watch_id is logged and returns nil. Transaction 1 loads the
// watch (gone or disabled: drop the mirror and stop) and debits the daily
// budget (over: reschedule and stop). Then the lead is resolved and each source
// runs in its OWN transaction (billing gate, fetch, side-channel patches, emit
// and cursor merge together, so a crash re-emits as a no-op). A finalize
// transaction records bootstrapped / attempt_count / last_polled_at and
// reschedules. Cancel or lease loss stops between sources, never mid-source.
func Handle(ctx context.Context, d HandlerDeps, c *queue.Claim) error {
	log := d.log()
	if !d.Enabled {
		return nil
	}
	var in pollPayloadIn
	var jobID int64
	if c != nil {
		jobID = c.ID
		if len(c.Payload) > 0 {
			dec := json.NewDecoder(bytes.NewReader(c.Payload))
			if err := dec.Decode(&in); err != nil {
				in = pollPayloadIn{}
			}
		}
	}
	if c == nil || c.WorkspaceID == "" {
		// The tenant comes from the job row only (plan 15 cutover); a job without one is refused.
		return &queue.PermanentError{Err: errors.New("watch_poll: the job has no workspace_id")}
	}
	ws, watchID := strOf(in.WorkspaceID), strOf(in.WatchID)
	if ws == "" || watchID == "" {
		log.Warn("watch_poll missing workspace_id/watch_id", "job_id", jobID)
		return nil
	}
	if c.WorkspaceID != ws {
		// A job queued under one workspace must not run inside another tenant.
		log.Error("watch_poll payload workspace does not match the job's workspace; refusing", "job_id", jobID)
		return nil
	}
	if !d.PGStore {
		log.Error("watch_poll requires the PG lead store (on_signal is PG-only); skipping", "job_id", jobID)
		return nil
	}
	fireKey := strOf(in.FireKey)
	if fireKey == "" {
		fireKey = "job:" + strconv.FormatInt(jobID, 10)
	}
	pollNow := providers.Truthy(in.PollNow)
	started := time.Now()
	pool := d.Pool

	// ── transaction 1: load, budget ──
	var kinds []string
	var stop bool
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		w, ok, err := LoadWatch(ctx, tx, watchID, ws)
		if err != nil {
			return err
		}
		if !ok || !w.Enabled {
			stop = true
			return mirrorDeleteScoped(ctx, tx, watchID, ws)
		}
		n, _, err := DebitBudget(ctx, tx, ws, pollNow, d.now())
		if err != nil {
			return err
		}
		if OverBudget(n, d.Cfg.DailyPollBudget) {
			log.Info("poller budget exceeded", "workspace_id", ws, "n", n, "budget", d.Cfg.DailyPollBudget)
			stop = true
			_, err := RescheduleWatch(ctx, tx, w, false, d.now(), d.Cfg, d.executor(ws))
			return err
		}
		row, ok, err := loadWatchRow(ctx, tx, watchID, ws)
		if err != nil {
			return err
		}
		if !ok {
			stop = true
			return nil
		}
		kinds = SourceSet(row.Kind, row.SignalTypes, d.Cfg.WebsiteTech)
		return nil
	})
	if err != nil {
		return err
	}
	if stop {
		return nil
	}

	// ── lead resolution (tenant-scoped) ──
	var lead int64
	ambiguous := false
	var gone bool
	err = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		row, ok, err := loadWatchRow(ctx, tx, watchID, ws)
		if err != nil {
			return err
		}
		if !ok {
			gone = true
			return mirrorDeleteScoped(ctx, tx, watchID, ws)
		}
		l, errText, err := ResolveLeadID(ctx, tx, row)
		if err != nil {
			return err
		}
		lead = l
		if errText != "ambiguous_lead" {
			return nil
		}
		ambiguous = true
		w, ok, err := LoadWatch(ctx, tx, watchID, ws)
		if err != nil || !ok {
			return err
		}
		now := d.now().UTC().Truncate(time.Microsecond)
		if err := gen.New(tx).PollEngineSetPolled(ctx, gen.PollEngineSetPolledParams{ID: watchID, WorkspaceID: ws, PolledAt: tsz(&now)}); err != nil {
			return fmt.Errorf("poller: set polled: %w", err)
		}
		le := "ambiguous_lead"
		w.LastError = &le
		_, err = RescheduleWatch(ctx, tx, w, false, d.now(), d.Cfg, d.executor(ws))
		return err
	})
	if err != nil {
		return err
	}
	if ambiguous || gone {
		return nil
	}

	// ── per source, ONE transaction each ──
	anyFailure := false
	var emitted, dupe int
	for _, src := range kinds {
		if c != nil {
			if cancelled, cerr := queue.IsCancelled(ctx, pool, c); cerr == nil && cancelled {
				log.Info("watch_poll cancelled between sources", "job_id", jobID, "watch_id", watchID)
				return nil
			}
			active, herr := queue.Heartbeat(ctx, pool, c)
			if herr == nil && !active {
				log.Warn("watch_poll lease lost between sources", "job_id", jobID, "watch_id", watchID)
				return nil
			}
		}
		out, err := d.pollOneSource(ctx, watchID, ws, src, fireKey, lead)
		if err != nil {
			return err
		}
		if out.failed {
			anyFailure = true
		}
		emitted += out.inserted
		dupe += out.dupe
	}

	// ── finalize ──
	err = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		w, ok, err := LoadWatch(ctx, tx, watchID, ws)
		if err != nil {
			return err
		}
		if !ok {
			return mirrorDeleteScoped(ctx, tx, watchID, ws)
		}
		q := gen.New(tx)
		st, err := q.PollEngineLock(ctx, gen.PollEngineLockParams{ID: watchID, WorkspaceID: ws})
		if err != nil {
			return fmt.Errorf("poller: lock watch: %w", err)
		}
		cur := decodeObject(st.Cursor)
		cur["bootstrapped"] = true
		attempts := int64(0)
		switch t := cur["attempt_count"].(type) {
		case int64:
			attempts = t
		case float64:
			attempts = int64(t)
		}
		cur["attempt_count"] = attempts + 1
		raw, err := encodeJSON(cur)
		if err != nil {
			return err
		}
		if err := q.PollEngineSaveCursor(ctx, gen.PollEngineSaveCursorParams{ID: watchID, WorkspaceID: ws, PollCursor: raw}); err != nil {
			return fmt.Errorf("poller: save cursor: %w", err)
		}
		now := d.now().UTC().Truncate(time.Microsecond)
		if err := q.PollEngineSetPolled(ctx, gen.PollEngineSetPolledParams{ID: watchID, WorkspaceID: ws, PolledAt: tsz(&now)}); err != nil {
			return fmt.Errorf("poller: set polled: %w", err)
		}
		if !anyFailure {
			w.LastError = nil
		}
		_, err = RescheduleWatch(ctx, tx, w, anyFailure, d.now(), d.Cfg, d.executor(ws))
		return err
	})
	if err != nil {
		return err
	}
	log.Info("watch_poll done", "watch_id", watchID, "workspace_id", ws, "emitted", emitted, "dupe", dupe,
		"failure", anyFailure, "dur_ms", time.Since(started).Milliseconds())
	return nil
}

// pollOneSource runs one source in three phases so no transaction spans the
// external fetch (a fetch can take a minute; holding a pooled connection and
// the workspace balance row lock that long would starve other work):
// tx A (short) loads the watch, runs the billing gate (idempotent per run id,
// commits even when the fetch later fails, like Python) and reads what the
// fetch needs (lead website, materialised contacts); the fetch runs with NO
// transaction; tx B (short) re-reads the watch under its row lock, applies the
// side-channel patches, emits and merges the cursor together. A returned error
// is a database or cancellation problem, never a fetch problem.
func (d HandlerDeps) pollOneSource(ctx context.Context, watchID, ws, src, fireKey string, lead int64) (sourceOutcome, error) {
	var (
		row      WatchRow
		contacts []Contact
		website  string
		terminal bool
		outcome  sourceOutcome
	)
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		var ok bool
		var err error
		row, ok, err = loadWatchRow(ctx, tx, watchID, ws)
		if err != nil || !ok {
			terminal = true
			return err
		}
		proceed, err := DebitSource(ctx, tx, d.Cfg, ws, src, fireKey)
		if err != nil {
			return err
		}
		if !proceed {
			terminal, outcome = true, sourceOutcome{failed: true}
			st, err := q.PollEngineLock(ctx, gen.PollEngineLockParams{ID: watchID, WorkspaceID: ws})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("poller: lock watch: %w", err)
			}
			msg := "insufficient_credits"
			return q.PollEngineSaveSource(ctx, gen.PollEngineSaveSourceParams{ID: watchID, WorkspaceID: ws,
				PollCursor: nonEmptyJSON(st.Cursor), ResolvedCik: st.ResolvedCik, LastError: textOf(&msg)})
		}
		switch src {
		case "web_tech":
			if lead != 0 && lead <= math.MaxInt32 {
				w, err := q.PollEngineLeadWebsite(ctx, gen.PollEngineLeadWebsiteParams{ID: int32(lead), WorkspaceID: ws})
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("poller: lead website: %w", err)
				}
				website = w.String
			}
		case "job_change":
			view := WatchView{ID: row.ID, Target: row.Target, Cursor: row.Cursor, ResolvedCIK: row.ResolvedCIK}
			contacts, err = MaterializeContacts(ctx, tx, PyWatch{WatchView: view, WorkspaceID: ws, FireKey: fireKey, Config: row.Config, SignalTypes: row.SignalTypes})
			return err
		}
		return nil
	})
	if err != nil || terminal {
		return outcome, err
	}

	view := WatchView{ID: row.ID, Target: row.Target, Cursor: row.Cursor, ResolvedCIK: row.ResolvedCIK}
	pyw := PyWatch{WatchView: view, WorkspaceID: ws, FireKey: fireKey, Config: row.Config, SignalTypes: row.SignalTypes}
	var evs []DetectedEvent
	var patch map[string]any
	var ferr error
	handled := false
	if d.Fetch != nil {
		evs, patch, handled, ferr = d.Fetch(ctx, src, pyw, lead, d.Backfill)
	}
	switch {
	case handled:
	case src == "funding" || src == "exec" || src == "funding_exec":
		evs, patch, ferr = FetchFundingAndExec(ctx, d.SourceDeps, view, src != "exec", src != "funding", d.Backfill)
	case src == "hiring" || src == "tech":
		evs, patch, ferr = FetchHiringAndTech(ctx, d.PyDeps, pyw, src == "hiring", src == "tech", d.Backfill)
	case src == "web_tech":
		evs, patch, ferr = FetchWebTech(ctx, d.SourceDeps, view, website, d.Backfill)
	case src == "feed":
		evs, patch, ferr = FetchFeedSource(ctx, d.SourceDeps, view, d.Backfill)
	case src == "job_change":
		evs, patch, ferr = FetchJobChanges(ctx, d.PyDeps, pyw, contacts, d.Backfill)
	case src == "account_group":
		evs, patch, ferr = FetchAccountGroup(ctx, d.PyDeps, pyw, d.Backfill)
	default:
		return sourceOutcome{}, nil
	}
	if ctx.Err() != nil {
		return sourceOutcome{}, ctx.Err() // shutdown: nothing applied
	}
	if ferr != nil {
		d.log().Warn("watch_poll source failed", "watch_id", watchID, "source", src, "err", ferr.Error())
		return sourceOutcome{failed: true}, nil // nothing advanced; the debit already committed like Python's
	}

	err = tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		var err error
		outcome, err = d.applySource(ctx, tx, row, src, lead, evs, patch)
		return err
	})
	return outcome, err
}

// applySource is tx B: re-read under the row lock, side-channel patches,
// unresolved-lead guard, emission and the cursor merge.
func (d HandlerDeps) applySource(ctx context.Context, tx pgx.Tx, row WatchRow, src string, lead int64, evs []DetectedEvent, patch map[string]any) (sourceOutcome, error) {
	q := gen.New(tx)
	watchID, ws := row.ID, row.WorkspaceID
	lockState := func() (gen.PollEngineLockRow, error) {
		st, err := q.PollEngineLock(ctx, gen.PollEngineLockParams{ID: watchID, WorkspaceID: ws})
		if err != nil {
			return st, fmt.Errorf("poller: lock watch: %w", err)
		}
		return st, nil
	}
	st, err := lockState()
	if errors.Is(err, pgx.ErrNoRows) {
		return sourceOutcome{}, nil // deleted mid-poll: write nothing
	}
	if err != nil {
		return sourceOutcome{}, err
	}
	resolved := st.ResolvedCik
	lastErr := st.LastError
	if patch != nil {
		patch = copyMap(patch)
	}
	if rc, ok := patch["_resolved_cik"]; ok {
		delete(patch, "_resolved_cik")
		if s, _ := rc.(string); s != "" && !(resolved.Valid && resolved.String != "") {
			resolved = textOf(&s)
		}
	}
	var failures []any
	if cf, ok := patch["_collector_failures"]; ok {
		delete(patch, "_collector_failures")
		failures, _ = cf.([]any)
		if fs, ok := cf.([]string); ok {
			for _, s := range fs {
				failures = append(failures, s)
			}
		}
		if len(failures) > 0 {
			msg := runeCut(pyStr(failures[0]), 255)
			lastErr = textOf(&msg)
		}
	}
	routed := src == "job_change" || src == "account_group"
	if len(evs) > 0 && lead == 0 && !routed {
		// A detected company event without a lead cannot be published: keep the cursor.
		msg := "unresolved_lead"
		if err := q.PollEngineSaveSource(ctx, gen.PollEngineSaveSourceParams{ID: watchID, WorkspaceID: ws,
			PollCursor: nonEmptyJSON(st.Cursor), ResolvedCik: resolved, LastError: textOf(&msg)}); err != nil {
			return sourceOutcome{}, fmt.Errorf("poller: save source: %w", err)
		}
		return sourceOutcome{failed: true}, nil
	}
	var counts EmitCounts
	if len(evs) > 0 && (lead != 0 || routed) {
		erow := row
		erow.ResolvedCIK = resolved.String
		counts, err = EmitEvents(ctx, tx, d.Hook, erow, lead, evs, d.now())
		if err != nil {
			return sourceOutcome{}, err
		}
	}
	cur := decodeObject(st.Cursor)
	for k, v := range patch {
		cur[k] = v
	}
	raw, err := encodeJSON(cur)
	if err != nil {
		return sourceOutcome{}, fmt.Errorf("poller: encode cursor: %w", err)
	}
	if err := q.PollEngineSaveSource(ctx, gen.PollEngineSaveSourceParams{ID: watchID, WorkspaceID: ws,
		PollCursor: raw, ResolvedCik: resolved, LastError: lastErr}); err != nil {
		return sourceOutcome{}, fmt.Errorf("poller: save source: %w", err)
	}
	return sourceOutcome{failed: len(failures) > 0, inserted: counts.Inserted, dupe: counts.Dupe}, nil
}

func textOf(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func nonEmptyJSON(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}")
	}
	return raw
}

func pyStr(v any) string { return providers.Str(v) }

// runeCut is s[:n] over characters.
func runeCut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func mirrorDeleteScoped(ctx context.Context, tx pgx.Tx, watchID, ws string) error {
	if err := gen.New(tx).PollEngineMirrorDelete(ctx, gen.PollEngineMirrorDeleteParams{WatchID: watchID, WorkspaceID: ws}); err != nil {
		return fmt.Errorf("poller: mirror delete: %w", err)
	}
	return nil
}
