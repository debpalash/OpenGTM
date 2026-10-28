package copilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// This file ports services/signals/tracking.py upsert_account_signal_schedule (the track_account_signals
// tool): one account_group watch per exact account scope, created or updated idempotently, scheduled through
// the poller's bootstrap (first aligned watch_poll job + watch_schedules mirror), then read back.

type trackingError struct{ msg string }

func (e *trackingError) Error() string { return e.msg }

func trackErr(msg string) error { return &trackingError{msg} }

// pyText is tracking._text: str(v).strip(), "" for None.
func pyText(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimFunc(pyStr(v), isPySpaceRune)
}

// urlHostname is urllib.parse.urlsplit(raw).hostname for the "<scheme>://" inputs built below.
func urlHostname(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	netloc := raw[i+3:]
	if j := strings.IndexAny(netloc, "/?#"); j >= 0 {
		netloc = netloc[:j]
	}
	if j := strings.LastIndex(netloc, "@"); j >= 0 {
		netloc = netloc[j+1:]
	}
	if strings.HasPrefix(netloc, "[") {
		if j := strings.Index(netloc, "]"); j >= 0 {
			return strings.ToLower(netloc[1:j])
		}
		return ""
	}
	if j := strings.Index(netloc, ":"); j >= 0 {
		netloc = netloc[:j]
	}
	return strings.ToLower(netloc)
}

// canonicalDomain is tracking._canonical_domain.
func canonicalDomain(v any) string {
	raw := pyLowerText(pyText(v))
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	host := strings.TrimRight(urlHostname(raw), ".")
	return strings.TrimPrefix(host, "www.")
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// strList is [str(v) for v in (x or [])]; ok=false where Python would iterate something else.
func strList(v any) ([]string, bool) {
	if !pyTruthy(v) {
		return []string{}, true
	}
	switch x := v.(type) {
	case []any:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = pyStr(e)
		}
		return out, true
	case string:
		var out []string
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, true
	}
	return nil, false
}

func (t *Turn) trackAccountSignals(ctx context.Context, a map[string]any) (string, bool, error) {
	e := t.Engine
	fail := func(msg string) (string, bool, error) {
		o := wbcrud.NewObj()
		o.Set("ok", false)
		o.Set("persisted", false)
		o.Set("error", msg)
		s, err := objJSON(o)
		return s, err == nil, err
	}
	if !e.IntentPollerEnabled {
		return fail("Intent signal tracking is disabled")
	}
	if !e.PGLeadStore {
		return fail("Intent signal tracking requires the Postgres lead store")
	}
	str := func(k string) string {
		if v := a[k]; pyTruthy(v) {
			return pyStr(v)
		}
		return ""
	}
	accounts, ok1 := strList(a["account_ids"])
	signals, ok2 := strList(a["signal_types"])
	if !ok1 || !ok2 {
		return "", false, nil
	}
	var out string
	err := t.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = t.upsertSignalSchedule(ctx, tx, str("workbook_id"), accounts, str("cadence"), signals, str("idempotency_key"))
		return err
	})
	var te *trackingError
	if errors.As(err, &te) {
		return fail(te.msg)
	}
	return out, err == nil, err
}

type trackedAccount struct {
	id     string
	obj    *wbcrud.Obj
	domain string
}

// loadAccounts is tracking._load_accounts.
func loadAccounts(ctx context.Context, tx pgx.Tx, ws, wbID string, ids []string) (string, []*wbcrud.Obj, error) {
	var name string
	err := tx.QueryRow(ctx, `SELECT name FROM workbooks WHERE id = $1 AND workspace_id = $2`, wbID, ws).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, trackErr("Workbook not found")
	}
	if err != nil {
		return "", nil, err
	}
	if len(ids) == 0 || slices.ContainsFunc(ids, func(s string) bool { return pyText(s) == "" }) {
		return "", nil, trackErr("Select at least one persisted account")
	}
	selected := map[string]bool{}
	for _, id := range ids {
		if selected[id] {
			return "", nil, trackErr("Account selection contains duplicate IDs")
		}
		selected[id] = true
	}
	rows, err := tx.Query(ctx, `SELECT data::text, canonical_entity_id, lead_id FROM record_workbook_rows
		WHERE workbook_id = $1 AND workspace_id = $2 ORDER BY position ASC, id ASC`, wbID, ws)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	byID := map[string]*wbcrud.Obj{}
	for rows.Next() {
		var data, canon *string
		var lead *int64
		if err := rows.Scan(&data, &canon, &lead); err != nil {
			return "", nil, err
		}
		d := map[string]any{}
		if data != nil {
			if v, err := canonjson.Decode([]byte(*data)); err == nil {
				if m, ok := v.(map[string]any); ok {
					d = m
				}
			}
		}
		var idv any = d["account_id"]
		if !pyTruthy(idv) && canon != nil {
			idv = *canon
		}
		id := pyText(idv)
		if !selected[id] {
			continue
		}
		company := pyText(d["company"])
		dv := d["canonical_domain"]
		if !pyTruthy(dv) {
			dv = d["website"]
		}
		domain := canonicalDomain(dv)
		if company == "" || domain == "" {
			label := id
			if label == "" {
				label = "<missing>"
			}
			return "", nil, trackErr("Account " + label + " lacks a company name or canonical domain")
		}
		o := wbcrud.NewObj()
		o.Set("account_id", id)
		o.Set("company", company)
		o.Set("canonical_domain", domain)
		o.Set("website", "https://"+domain)
		if lead != nil {
			o.Set("lead_id", *lead)
		} else {
			o.Set("lead_id", nil)
		}
		byID[id] = o
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	var missing []string
	for _, id := range ids {
		if byID[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return "", nil, trackErr("Account selection drifted; missing persisted IDs: " + strings.Join(missing[:min(5, len(missing))], ", "))
	}
	out := make([]*wbcrud.Obj, len(ids))
	for i, id := range ids {
		out[i] = byID[id]
	}
	return name, out, nil
}

type watchRow struct {
	id, interval   string
	signalTypes    []string
	enabled        bool
	nextPoll       *time.Time
	anchor         *time.Time
	config, cursor *wbcrud.Obj
	lastError      *string
}

func parseObjText(s *string) *wbcrud.Obj {
	if s != nil && *s != "null" {
		if o, err := wbcrud.ParseObject([]byte(*s)); err == nil {
			return o
		}
	}
	return wbcrud.NewObj()
}

func (t *Turn) upsertSignalSchedule(ctx context.Context, tx pgx.Tx, wbID string, accountIDs []string, cadence string, signals []string, key string) (string, error) {
	cadence = pyLowerText(pyText(cadence))
	if !slices.Contains(AccountSignalCadences, cadence) {
		c := cadence
		if c == "" {
			c = "<missing>"
		}
		return "", trackErr("Unsupported cadence: " + c)
	}
	var normalized []string
	for _, s := range AccountSignalTypes {
		if slices.Contains(signals, s) {
			normalized = append(normalized, s)
		}
	}
	uniq := map[string]bool{}
	for _, s := range signals {
		uniq[s] = true
	}
	if len(normalized) == 0 || len(normalized) != len(signals) || len(signals) != len(uniq) {
		return "", trackErr("Signal types must be unique supported account signals")
	}
	actionKey := pyText(key)
	if actionKey == "" || runeLen(actionKey) > 255 {
		return "", trackErr("A valid idempotency key is required")
	}
	ids := make([]string, len(accountIDs))
	for i, v := range accountIDs {
		ids[i] = pyText(v)
	}
	wbName, accounts, err := loadAccounts(ctx, tx, t.WS, wbID, ids)
	if err != nil {
		return "", err
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	scopeKey := "accounts_" + sha256hex(strings.Join(sorted, "|"))[:24]
	sortedSignals := append([]string(nil), normalized...)
	sort.Strings(sortedSignals)
	contract := sha256hex(strings.Join(append(append(append([]string{wbID}, sorted...), cadence), sortedSignals...), "|"))

	rows, err := tx.Query(ctx, `SELECT id, coalesce(interval, ''), signal_types::text, enabled, next_poll_at, schedule_anchor,
		config::text, cursor::text, last_error FROM watch_subscriptions WHERE workspace_id = $1 AND kind = 'account_group'
		ORDER BY created_at, id FOR UPDATE`, t.WS)
	if err != nil {
		return "", err
	}
	var existing *watchRow
	var conflict bool
	for rows.Next() {
		var w watchRow
		var st, cfg, cur *string
		if err := rows.Scan(&w.id, &w.interval, &st, &w.enabled, &w.nextPoll, &w.anchor, &cfg, &cur, &w.lastError); err != nil {
			rows.Close()
			return "", err
		}
		w.config, w.cursor = parseObjText(cfg), parseObjText(cur)
		if st != nil {
			if v, err := canonjson.Decode([]byte(*st)); err == nil {
				w.signalTypes, _ = strList(v)
			}
		}
		if reqs, ok := w.config.Get("action_requests"); ok {
			if ro, ok := reqs.(*wbcrud.Obj); ok {
				if h, has := ro.Get(actionKey); has && h != contract {
					conflict = true
				}
			}
		}
		if sk, _ := w.config.Get("scope_key"); sk == scopeKey {
			wc := w
			existing = &wc
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if conflict {
		return "", trackErr("Idempotency key already belongs to a different tracking request")
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	reused, updated := existing != nil, false
	w := existing
	sigJSON, err := pyJSONText(anyStrings(normalized))
	if err != nil {
		return "", err
	}
	if w == nil {
		w = &watchRow{id: uuid.NewString(), interval: cadence, signalTypes: normalized, enabled: true, anchor: &now,
			config: wbcrud.NewObj(), cursor: wbcrud.NewObj()}
		w.cursor.Set("bootstrapped", false)
		w.cursor.Set("attempt_count", int64(0))
		w.cursor.Set("collector_health", wbcrud.NewObj())
		curJSON, err := pyJSONText(w.cursor)
		if err != nil {
			return "", err
		}
		target := "Account set " + strings.TrimPrefix(scopeKey, "accounts_")[:8]
		if _, err := tx.Exec(ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, signal_types, interval, config,
			schedule_anchor, enabled, cursor, consecutive_failures, created_at, updated_at)
			VALUES ($1, $2, 'account_group', $3, $4::json, $5, '{}'::json, $6, true, $7::json, 0, now(), now())`,
			w.id, t.WS, target, sigJSON, cadence, now, curJSON); err != nil {
			return "", err
		}
	} else {
		updated = w.interval != cadence || !slices.Equal(w.signalTypes, normalized) || !w.enabled
		wasDisabled := !w.enabled
		w.interval, w.signalTypes, w.enabled = cadence, normalized, true
		if updated {
			w.anchor = &now
		}
		if _, err := tx.Exec(ctx, `UPDATE watch_subscriptions SET interval = $2, signal_types = $3::json, enabled = true,
			consecutive_failures = CASE WHEN $4 THEN 0 ELSE consecutive_failures END,
			last_error = CASE WHEN $4 THEN NULL ELSE last_error END, schedule_anchor = $5, updated_at = now()
			WHERE id = $1`, w.id, cadence, sigJSON, wasDisabled, w.anchor); err != nil {
			return "", err
		}
		if wasDisabled {
			w.lastError = nil
		}
	}
	requests := wbcrud.NewObj()
	if r, ok := w.config.Get("action_requests"); ok {
		if ro, ok := r.(*wbcrud.Obj); ok {
			requests = ro
		}
	}
	requests.Set(actionKey, contract)
	accAny := make([]any, len(accounts))
	for i, o := range accounts {
		accAny[i] = o
	}
	for _, kv := range []struct {
		k string
		v any
	}{{"scope_key", scopeKey}, {"workbook_id", wbID}, {"workbook_name", wbName}, {"accounts", accAny},
		{"requested_signal_types", anyStrings(normalized)}, {"action_requests", requests}, {"updated_at", collectledger.Isoformat(now)}} {
		w.config.Set(kv.k, kv.v)
	}
	cfgJSON, err := pyJSONText(w.config)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE watch_subscriptions SET config = $2::json WHERE id = $1`, w.id, cfgJSON); err != nil {
		return "", err
	}
	if updated {
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'cancelled', completed_at = (now() AT TIME ZONE 'utc'), error = 'schedule_updated'
			WHERE type = 'watch_poll' AND status IN ('pending', 'processing') AND payload->>'watch_id' = $1`, w.id); err != nil {
			return "", err
		}
	}
	if !reused || updated || w.nextPoll == nil {
		anchor := now
		if w.anchor != nil {
			anchor = *w.anchor
		}
		pw := poller.Watch{ID: w.id, WorkspaceID: t.WS, Interval: w.interval, Anchor: &anchor, Enabled: true}
		// Python bootstraps after the anchor was taken, so a fresh anchor yields anchor + one interval.
		at := time.Now().UTC()
		if !at.After(anchor) {
			at = anchor.Add(time.Microsecond)
		}
		next := poller.NextPollFor(pw, at, poller.Config{DefaultInterval: "daily"})
		if _, err := poller.EnqueuePollIfAbsent(ctx, tx, poller.ScheduledFireKey(w.id, next), t.WS, w.id, next, false, jobExecutor); err != nil {
			return "", err
		}
		if err := poller.MirrorSet(ctx, tx, w.id, t.WS, &next, true); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE watch_subscriptions SET next_poll_at = $2 WHERE id = $1`, w.id, next); err != nil {
			return "", err
		}
	}
	return scheduleReceipt(ctx, tx, t.WS, w.id, actionKey, reused, updated)
}

func anyStrings(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// scheduleReceipt is schedule_receipt over the row read back from storage, plus action_id, reused, updated.
func scheduleReceipt(ctx context.Context, tx pgx.Tx, ws, id, actionKey string, reused, updated bool) (string, error) {
	var interval, st, cfg, cur, lastError *string
	var enabled bool
	var next *time.Time
	err := tx.QueryRow(ctx, `SELECT interval, signal_types::text, enabled, next_poll_at, config::text, cursor::text, last_error
		FROM watch_subscriptions WHERE id = $1 AND workspace_id = $2`, id, ws).Scan(&interval, &st, &enabled, &next, &cfg, &cur, &lastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", trackErr("Schedule write could not be confirmed")
	}
	if err != nil {
		return "", err
	}
	config, cursor := parseObjText(cfg), parseObjText(cur)
	cget := func(k string) any { v, _ := config.Get(k); return v }
	accounts, _ := cget("accounts").([]any)
	if accounts == nil {
		accounts = []any{}
	}
	accIDs := []any{}
	for _, item := range accounts {
		if o, ok := item.(*wbcrud.Obj); ok {
			v, _ := o.Get("account_id")
			accIDs = append(accIDs, pyText(v))
		}
	}
	var nextRun, nextRetry any
	if next != nil {
		nextRun = collectledger.Isoformat(next.UTC())
	}
	if lastError != nil && *lastError != "" && enabled {
		nextRetry = nextRun
	}
	state := "active"
	switch {
	case !enabled:
		state = "paused"
	case lastError != nil && *lastError != "":
		state = "degraded"
	}
	var sigs any = []any{}
	if st != nil {
		if v, err := canonjson.Decode([]byte(*st)); err == nil && v != nil {
			sigs = v
		}
	}
	attempts := int64(0)
	if v, ok := cursor.Get("attempt_count"); ok && pyTruthy(v) {
		attempts, _ = pyIntConv(jsonNum(v))
	}
	health := wbcrud.NewObj()
	if v, ok := cursor.Get("collector_health"); ok {
		if o, ok := v.(*wbcrud.Obj); ok {
			health = o
		}
	}
	scope := wbcrud.NewObj()
	scope.Set("account_count", len(accounts))
	scope.Set("account_ids", accIDs)
	scope.Set("accounts", accounts)
	retry := wbcrud.NewObj()
	retry.Set("method", "POST")
	retry.Set("url", "/api/watches/"+id+"/poll")
	retry.Set("label", "Retry collectors now")
	o := wbcrud.NewObj()
	o.Set("ok", true)
	o.Set("persisted", true)
	o.Set("schedule_id", id)
	o.Set("workspace_id", ws)
	o.Set("workbook_id", pyText(cget("workbook_id")))
	o.Set("scope_key", pyText(cget("scope_key")))
	o.Set("scope", scope)
	if interval != nil {
		o.Set("cadence", *interval)
	} else {
		o.Set("cadence", nil)
	}
	o.Set("signal_types", sigs)
	o.Set("next_run_at", nextRun)
	o.Set("state", state)
	o.Set("attempt_count", attempts)
	if lastError != nil {
		o.Set("last_error_class", *lastError)
	} else {
		o.Set("last_error_class", nil)
	}
	o.Set("next_retry_at", nextRetry)
	o.Set("collector_health", health)
	o.Set("manual_retry_action", retry)
	o.Set("readback_confirmed", true)
	o.Set("url", "/watches?id="+id)
	o.Set("action_id", actionKey)
	o.Set("reused", reused)
	o.Set("updated", updated)
	return objJSON(o)
}

// jsonNum converts a json.Number (wbcrud.ParseObject's number type) into the canonjson shapes.
func jsonNum(v any) any {
	if n, ok := v.(interface{ String() string }); ok {
		if d, err := canonjson.Decode([]byte(n.String())); err == nil {
			return d
		}
	}
	return v
}
