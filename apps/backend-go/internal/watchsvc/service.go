package watchsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Service is the watch store behind /api/watches.
type Service struct {
	Pool    *pgxpool.Pool
	Cfg     Config
	Secrets autosvc.SecretChecker
	// Outbound is the webhook_secret_ref policy (zero value: default prefixes, no registry deny-list).
	Outbound secrets.OutboundPolicy
	// ExecFor is go_executor.watch_poll_executor (nil: python).
	ExecFor func(ws string) string
	Now     func() time.Time
	Log     *slog.Logger
}

var errWatchNotFound = &HTTPError{Status: 404, Detail: "watch not found"}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

func (s *Service) executor(ws string) string {
	if s.ExecFor != nil && s.ExecFor(ws) == "go" {
		return "go"
	}
	return "python"
}

func (s *Service) pcfg() poller.Config { return poller.Config{DefaultInterval: s.Cfg.DefaultInterval} }

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// leadInWorkspace is the D4 pin check: a pinned lead must exist in the
// caller's workspace (RLS on leads plus the explicit predicate).
func leadInWorkspace(ctx context.Context, tx pgx.Tx, ws string, id int32) error {
	ok, err := gen.New(tx).WatchSvcLeadExists(ctx, gen.WatchSvcLeadExistsParams{ID: id, WorkspaceID: ws})
	if err != nil {
		return err
	}
	if !ok {
		return herr(422, "lead_id %d not found in this workspace", id)
	}
	return nil
}

// bootstrap is schedule_bootstrap_for_watch: the aligned next poll, the first
// scheduled job if absent and the mirror row, all in tx. It returns the next
// poll instant (the caller stores it on the watch).
func (s *Service) bootstrap(ctx context.Context, tx pgx.Tx, id, ws, interval string, anchor *time.Time, enabled bool) (time.Time, error) {
	now := s.now()
	iv := interval
	pw := poller.Watch{ID: id, WorkspaceID: ws, Interval: iv, Anchor: anchor, Enabled: enabled}
	next := poller.NextPollFor(pw, now, s.pcfg())
	if _, err := poller.EnqueuePollIfAbsent(ctx, tx, poller.ScheduledFireKey(id, next), ws, id, next, false, s.executor(ws)); err != nil {
		return next, err
	}
	if err := poller.MirrorSet(ctx, tx, id, ws, &next, enabled); err != nil {
		return next, err
	}
	return next, nil
}

// maybeWebhookRule is _maybe_create_webhook_rule: an on_signal -> webhook rule
// created in its OWN transaction (it stays when the watch insert later fails,
// as in Python). Returns nil when no rule was requested.
func (s *Service) maybeWebhookRule(ctx context.Context, ws string, in CreateInput, signalTypes []string) (*string, error) {
	if !in.CreateWebhookRule {
		return nil, nil
	}
	if !s.Cfg.AutomationsEnabled {
		return nil, herr(409, "automations_disabled_cannot_create_webhook_rule")
	}
	url := ""
	if in.WebhookURL != nil {
		url = colgraph.PyStrip(*in.WebhookURL)
	}
	if url == "" {
		return nil, herr(422, "webhook_url required when create_webhook_rule is set")
	}
	scheme, creds, host := urlParts(url)
	if scheme != "http" && scheme != "https" {
		return nil, herr(422, "webhook_url: only http/https allowed")
	}
	if creds {
		return nil, herr(422, "webhook_url: credentials not allowed")
	}
	if len(s.Cfg.WebhookAllowlist) > 0 && !autosvc.AllowlistMatch(host, s.Cfg.WebhookAllowlist) {
		return nil, herr(422, "webhook_url: host '%s' not in webhook domain allowlist", host)
	}
	cfg := wbcrud.NewObj()
	cfg.Set("url", url)
	cfg.Set("method", "POST")
	if in.WebhookSecretRef != nil && *in.WebhookSecretRef != "" {
		found := false
		// Same outbound-namespace policy as automation rules; not-allowed reads as not found.
		if s.Secrets != nil && secrets.CheckOutboundRef(*in.WebhookSecretRef, s.Outbound) == nil {
			var err error
			if found, err = s.Secrets.Has(ctx, ws, *in.WebhookSecretRef); err != nil {
				return nil, fmt.Errorf("watchsvc: secret lookup: %w", err)
			}
		}
		if !found {
			return nil, herr(422, "webhook_secret_ref not found")
		}
		cfg.Set("header_secret_ref", *in.WebhookSecretRef)
	}
	action := wbcrud.NewObj()
	action.Set("type", "webhook")
	action.Set("config", cfg)
	st := make([]any, len(signalTypes))
	for i, x := range signalTypes {
		st[i] = x
	}
	tc := wbcrud.NewObj()
	tc.Set("signal_types", st)
	tcJSON, _ := tc.MarshalJSON()
	actions := wbcrud.NewObj()
	actions.Set("v", []any{action})
	ab, _ := actions.MarshalJSON()
	ab = ab[len(`{"v":`) : len(ab)-1]
	var id string
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		id = autosvc.NewID()
		_, err := gen.New(tx).WatchSvcInsertWebhookRule(ctx, gen.WatchSvcInsertWebhookRuleParams{
			ID: id, WorkspaceID: ws, Name: webhookRuleName(in.Kind, in.Target),
			TriggerConfig: tcJSON, Actions: ab})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("watchsvc: create webhook rule: %w", err)
	}
	return &id, nil
}

// dropRule best-effort deletes the rule maybeWebhookRule created when the
// watch insert that followed it failed, so no unreachable live rule remains.
func (s *Service) dropRule(ctx context.Context, ws string, id *string) {
	if id == nil {
		return
	}
	cctx := context.WithoutCancel(ctx)
	err := tenancy.InWorkspace(cctx, s.Pool, ws, func(tx pgx.Tx) error {
		_, err := gen.New(tx).AutoSvcDeleteTrigger(cctx, gen.AutoSvcDeleteTriggerParams{ID: *id, WorkspaceID: ws})
		return err
	})
	if err != nil && s.Log != nil {
		s.Log.Warn("watches: drop orphan webhook rule failed", "err_type", fmt.Sprintf("%T", err))
	}
}

// urlParts returns scheme, whether credentials are present and the hostname
// (urllib.parse.urlparse semantics, via a tiny local parse).
func urlParts(raw string) (scheme string, creds bool, host string) {
	u := strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(strings.TrimLeftFunc(raw, func(r rune) bool { return r <= 0x20 }))
	if i := strings.IndexByte(u, ':'); i > 0 {
		cand := u[:i]
		ok := (cand[0] >= 'a' && cand[0] <= 'z') || (cand[0] >= 'A' && cand[0] <= 'Z')
		for j := 0; j < len(cand) && ok; j++ {
			b := cand[j]
			ok = (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '+' || b == '-' || b == '.'
		}
		if ok {
			scheme = strings.ToLower(cand)
			u = u[i+1:]
		}
	}
	if !strings.HasPrefix(u, "//") {
		return scheme, false, ""
	}
	netloc := u[2:]
	for _, d := range "/?#" {
		if j := strings.IndexRune(netloc, d); j >= 0 {
			netloc = netloc[:j]
		}
	}
	hostinfo := netloc
	if at := strings.LastIndexByte(netloc, '@'); at >= 0 {
		ui := netloc[:at]
		hostinfo = netloc[at+1:]
		if c := strings.IndexByte(ui, ':'); c >= 0 {
			creds = ui != "" && (ui[:c] != "" || ui[c+1:] != "")
		} else {
			creds = ui != ""
		}
	}
	if b := strings.IndexByte(hostinfo, '['); b >= 0 {
		hostinfo = hostinfo[b+1:]
		if e := strings.IndexByte(hostinfo, ']'); e >= 0 {
			hostinfo = hostinfo[:e]
		}
		return scheme, creds, strings.ToLower(hostinfo)
	}
	if c := strings.IndexByte(hostinfo, ':'); c >= 0 {
		hostinfo = hostinfo[:c]
	}
	return scheme, creds, strings.ToLower(hostinfo)
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// Create is POST /watches: validate, enforce the per-workspace cap, create the
// optional webhook rule (own transaction), then insert the watch with its
// first scheduled poll and mirror row. A duplicate (kind, target) is 409 (D4;
// Python 500s on the unique index).
func (s *Service) Create(ctx context.Context, ws string, in CreateInput) (Created, error) {
	v, err := ValidateCreate(in, s.Cfg)
	if err != nil {
		return Created{}, err
	}
	max := s.Cfg.MaxWatchesPerWS
	dupCheck := func(tx pgx.Tx) error {
		dup, err := gen.New(tx).WatchSvcExists(ctx, gen.WatchSvcExistsParams{WorkspaceID: ws, Kind: v.Kind, Target: v.Target})
		if err != nil {
			return err
		}
		if dup {
			return herr(409, "watch already exists for this kind and target")
		}
		return nil
	}
	checkCount := func(tx pgx.Tx) error {
		// F21: serialise cap check + insert per workspace (held to commit).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('watches:' || $1::text, 0))`, ws); err != nil {
			return err
		}
		n, err := gen.New(tx).WatchSvcCount(ctx, ws)
		if err != nil {
			return err
		}
		if int(n) >= max {
			return herr(422, "max watches per workspace reached (%d)", max)
		}
		return nil
	}
	// Cap and duplicate checks come before the webhook rule so a refused
	// create leaves no orphan rule behind.
	if err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		if err := checkCount(tx); err != nil {
			return err
		}
		if in.LeadID != nil { // before the rule: a refused pin must not leave one behind
			if err := leadInWorkspace(ctx, tx, ws, *in.LeadID); err != nil {
				return err
			}
		}
		return dupCheck(tx)
	}); err != nil {
		return Created{}, err
	}
	ruleID, err := s.maybeWebhookRule(ctx, ws, in, v.SignalTypes)
	if err != nil {
		return Created{}, err
	}
	var out Watch
	err = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		if err := checkCount(tx); err != nil {
			return err
		}
		if in.LeadID != nil {
			if err := leadInWorkspace(ctx, tx, ws, *in.LeadID); err != nil {
				return err
			}
		}
		id := autosvc.NewID()
		anchor := s.now()
		next, err := s.bootstrap(ctx, tx, id, ws, v.Interval, &anchor, true)
		if err != nil {
			return err
		}
		p := gen.WatchSvcInsertParams{ID: id, WorkspaceID: ws, Kind: v.Kind, Target: v.Target,
			SignalTypes: mustJSON(v.SignalTypes), PollInterval: v.Interval, ScheduleAnchor: tstz(anchor),
			NextPollAt: tstz(next), PollCursor: []byte(`{"bootstrapped":false}`)}
		if in.LeadID != nil {
			p.LeadID = pgtype.Int4{Int32: *in.LeadID, Valid: true}
		}
		if v.Config != nil {
			b, _ := v.Config.MarshalJSON()
			p.Config = b
		}
		row, err := gen.New(tx).WatchSvcInsert(ctx, p)
		if isUnique(err) {
			return herr(409, "watch already exists for this kind and target")
		}
		if err != nil {
			return fmt.Errorf("watchsvc: insert watch: %w", err)
		}
		out, err = toWatch(row)
		return err
	})
	if err != nil {
		s.dropRule(ctx, ws, ruleID) // the watch insert lost a race: do not leave the rule live
		return Created{}, err
	}
	return Created{Watch: out, WebhookRuleID: ruleID}, nil
}

// ListPage is GET /watches.
type ListPage struct {
	Watches    []Watch
	Limit      int
	Offset     *int // nil when a cursor was used
	HasMore    bool
	NextCursor *string
}

// MarshalJSON renders {watches, limit, offset, has_more, next_cursor}.
func (p ListPage) MarshalJSON() ([]byte, error) {
	o := wbcrud.NewObj()
	ws := make([]any, len(p.Watches))
	for i, w := range p.Watches {
		ws[i] = w.Obj()
	}
	o.Set("watches", ws)
	o.Set("limit", int64(p.Limit))
	if p.Offset == nil {
		o.Set("offset", nil)
	} else {
		o.Set("offset", int64(*p.Offset))
	}
	o.Set("has_more", p.HasMore)
	if p.NextCursor == nil {
		o.Set("next_cursor", nil)
	} else {
		o.Set("next_cursor", *p.NextCursor)
	}
	return o.MarshalJSON()
}

// List is GET /watches: newest first, keyset cursor or offset paging.
func (s *Service) List(ctx context.Context, ws string, limit, offset int, cursor string) (ListPage, error) {
	pg := ListPage{Limit: limit}
	p := gen.WatchSvcListParams{WorkspaceID: ws, RowLimit: int32(limit + 1)}
	if cursor != "" {
		ts, id, err := DecodeCursor(cursor)
		if err != nil {
			return pg, err
		}
		p.CursorTs = tstz(ts)
		p.CursorID = pgtype.Text{String: id, Valid: true}
	} else {
		p.RowOffset = int32(min(offset, math.MaxInt32))
		o := offset
		pg.Offset = &o
	}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).WatchSvcList(ctx, p)
		if err != nil {
			return err
		}
		pg.HasMore = len(rows) > limit
		if pg.HasMore {
			rows = rows[:limit]
		}
		pg.Watches = make([]Watch, len(rows))
		for i, r := range rows {
			if pg.Watches[i], err = toWatch(r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return pg, err
	}
	if pg.HasMore && len(pg.Watches) > 0 {
		c := cursorOf(pg.Watches[len(pg.Watches)-1])
		pg.NextCursor = &c
	}
	return pg, nil
}

// Get is GET /watches/{id}; unknown or foreign ids are 404 "watch not found".
func (s *Service) Get(ctx context.Context, ws, id string) (Watch, error) {
	var out Watch
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		row, err := gen.New(tx).WatchSvcGet(ctx, gen.WatchSvcGetParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWatchNotFound
		}
		if err != nil {
			return err
		}
		out, err = toWatch(row)
		return err
	})
	return out, err
}

// Patch is PATCH /watches/{id}. Enabling a disabled watch resets the failure
// state and re-bootstraps its schedule; disabling clears next_poll_at and the
// mirror's due time. All changes commit together (a rejected later field
// leaves nothing applied).
func (s *Service) Patch(ctx context.Context, ws, id string, in PatchInput) (Watch, error) {
	var out Watch
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.WatchSvcLock(ctx, gen.WatchSvcLockParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWatchNotFound
		}
		if err != nil {
			return err
		}
		ch, err := ValidatePatch(row.Kind, in, s.Cfg)
		if err != nil {
			return err
		}
		p := gen.WatchSvcSaveParams{ID: id, WorkspaceID: ws, PollInterval: row.Interval, Config: row.Config,
			SignalTypes: row.SignalTypes, LeadID: row.LeadID, Enabled: row.Enabled, NextPollAt: row.NextPollAt,
			ConsecutiveFailures: row.ConsecutiveFailures.Int32, LastError: row.LastError}
		if ch.Interval != nil {
			p.PollInterval = pgtype.Text{String: *ch.Interval, Valid: true}
		}
		if ch.Config != nil {
			b, _ := ch.Config.MarshalJSON()
			p.Config = b
		}
		if ch.SignalTypes != nil {
			p.SignalTypes = mustJSON(*ch.SignalTypes)
		}
		if in.LeadID != nil {
			if err := leadInWorkspace(ctx, tx, ws, *in.LeadID); err != nil {
				return err
			}
			p.LeadID = pgtype.Int4{Int32: *in.LeadID, Valid: true}
		}
		if in.Enabled != nil {
			was := row.Enabled
			p.Enabled = *in.Enabled
			switch {
			case *in.Enabled && !was:
				p.ConsecutiveFailures = 0
				p.LastError = pgtype.Text{}
				var anchor *time.Time
				if row.ScheduleAnchor.Valid {
					a := row.ScheduleAnchor.Time.UTC()
					anchor = &a
				}
				next, err := s.bootstrap(ctx, tx, id, ws, p.PollInterval.String, anchor, true)
				if err != nil {
					return err
				}
				p.NextPollAt = tstz(next)
			case !*in.Enabled:
				p.NextPollAt = pgtype.Timestamptz{}
				if err := poller.MirrorSet(ctx, tx, id, ws, nil, false); err != nil {
					return err
				}
			}
		}
		upd, err := q.WatchSvcSave(ctx, p)
		if err != nil {
			return fmt.Errorf("watchsvc: save watch: %w", err)
		}
		out, err = toWatch(upd)
		return err
	})
	return out, err
}

// Delete removes the schedule mirror first, then the watch.
func (s *Service) Delete(ctx context.Context, ws, id string) error {
	return tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.WatchSvcLock(ctx, gen.WatchSvcLockParams{ID: id, WorkspaceID: ws}); errors.Is(err, pgx.ErrNoRows) {
			return errWatchNotFound
		} else if err != nil {
			return err
		}
		if err := poller.MirrorDelete(ctx, tx, id); err != nil {
			return err
		}
		_, err := q.WatchSvcDelete(ctx, gen.WatchSvcDeleteParams{ID: id, WorkspaceID: ws})
		return err
	})
}
