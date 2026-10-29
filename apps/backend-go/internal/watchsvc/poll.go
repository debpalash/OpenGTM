package watchsvc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/text/cases"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signals"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// PollResult is the POST /watches/{id}/poll answer.
type PollResult struct {
	Status  string // "queued" or "already_queued"
	JobID   *int64 // set when an active scheduled poll was reused
	FireKey string
}

// MarshalJSON renders {status, [job_id,] fire_key}.
func (r PollResult) MarshalJSON() ([]byte, error) {
	o := wbcrud.NewObj()
	o.Set("status", r.Status)
	if r.JobID != nil {
		o.Set("job_id", *r.JobID)
	}
	o.Set("fire_key", r.FireKey)
	return o.MarshalJSON()
}

// PollNow is POST /watches/{id}/poll. 409 for a disabled watch, 429 inside the
// per-watch minimum interval or past the daily poll-now quota, an active
// scheduled poll is reused (already_queued), otherwise a manual poll keyed
// watch:<id>:pollnow:<UTC date> is enqueued with poll_now in its payload.
func (s *Service) PollNow(ctx context.Context, ws, id string) (PollResult, error) {
	var res PollResult
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.WatchSvcLock(ctx, gen.WatchSvcLockParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWatchNotFound
		}
		if err != nil {
			return err
		}
		now := s.now()
		if !row.Enabled {
			return herr(409, "watch disabled")
		}
		if min := s.Cfg.PollNowMinIntervalSec; min > 0 && row.LastPolledAt.Valid {
			if now.Sub(row.LastPolledAt.Time.UTC()).Seconds() < float64(min) {
				return herr(429, "poll-now rate limited")
			}
		}
		if quota := s.Cfg.PollNowDailyQuota; quota > 0 {
			day := now.Format("2006-01-02")
			// Serialise check + enqueue per (workspace, UTC day): the debit
			// happens at execution, so queued manual polls count too.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('pollnow:' || $1::text || ':' || $2::text))`, ws, day); err != nil {
				return err
			}
			used, err := poller.PollNowQuotaUsed(ctx, tx, ws, now)
			if err != nil {
				return err
			}
			pending, err := q.WatchSvcPendingPollNow(ctx, gen.WatchSvcPendingPollNowParams{WorkspaceID: ws, Day: day})
			if err != nil {
				return err
			}
			if used+int(pending) >= quota {
				return herr(429, "poll-now daily quota exhausted")
			}
		}
		jobs, err := q.WatchSvcActiveScheduledJobs(ctx, "watch:"+id+":")
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if !strings.Contains(j.FireKey.String, ":pollnow:") {
				jid := int64(j.ID)
				res = PollResult{Status: "already_queued", JobID: &jid, FireKey: j.FireKey.String}
				return nil
			}
		}
		key := "watch:" + id + ":pollnow:" + now.Format("2006-01-02")
		created, err := poller.EnqueuePollIfAbsent(ctx, tx, key, ws, id, now, true, s.executor(ws))
		if err != nil {
			return err
		}
		res = PollResult{Status: "queued", FireKey: key}
		if !created {
			res.Status = "already_queued"
		}
		return nil
	})
	return res, err
}

// SignalsQuery is the GET /watches/{id}/signals query (already range-checked
// by the route: limit 1..200, offset >= 0).
type SignalsQuery struct {
	SignalType string
	Limit      int
	Offset     int
	Cursor     string
}

// SignalsPage is the signal feed answer: {signals, next_cursor, has_more,
// limit, offset}.
type SignalsPage struct {
	Signals    []signals.Signal
	NextCursor *string
	HasMore    bool
	Limit      int
	Offset     *int
}

// MarshalJSON renders the page in Python key order.
func (p SignalsPage) MarshalJSON() ([]byte, error) {
	o := wbcrud.NewObj()
	sigs := make([]any, len(p.Signals))
	for i := range p.Signals {
		sigs[i] = p.Signals[i]
	}
	o.Set("signals", sigs)
	if p.NextCursor == nil {
		o.Set("next_cursor", nil)
	} else {
		o.Set("next_cursor", *p.NextCursor)
	}
	o.Set("has_more", p.HasMore)
	o.Set("limit", int64(p.Limit))
	if p.Offset == nil {
		o.Set("offset", nil)
	} else {
		o.Set("offset", int64(*p.Offset))
	}
	return o.MarshalJSON()
}

func sigFrom(r gen.Signal) signals.Signal {
	i64 := func(v pgtype.Int4) *int64 {
		if !v.Valid {
			return nil
		}
		n := int64(v.Int32)
		return &n
	}
	tx := func(t pgtype.Text) *string {
		if !t.Valid {
			return nil
		}
		return &t.String
	}
	s := signals.Signal{ID: r.ID, WorkspaceID: r.WorkspaceID, LeadID: i64(r.LeadID), Company: tx(r.Company),
		SignalType: tx(r.SignalType), Title: tx(r.Title), Description: tx(r.Description), Source: tx(r.Source),
		SourceURL: tx(r.SourceUrl), Weight: i64(r.Weight)}
	if r.CreatedAt.Valid {
		f := r.CreatedAt.Float64
		s.CreatedAt = &f
	}
	if r.Read.Valid && r.Read.Bool {
		s.Read = 1
	}
	return s
}

// Signals is GET /watches/{id}/signals: the watch's signal types (or the
// requested one), pinned lead, and for account_group watches the lead/company
// expansion of its accounts.
func (s *Service) Signals(ctx context.Context, ws, id string, q SignalsQuery) (SignalsPage, error) {
	pg := SignalsPage{Limit: q.Limit}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		row, err := gen.New(tx).WatchSvcGet(ctx, gen.WatchSvcGetParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWatchNotFound
		}
		if err != nil {
			return err
		}
		w, err := toWatch(row)
		if err != nil {
			return err
		}
		p := gen.WatchSvcSignalsPageParams{WorkspaceID: ws, RowLimit: int32(q.Limit + 1), LeadIds: []int32{}, Companies: []string{}}
		if q.SignalType != "" {
			p.SignalTypes = []string{q.SignalType}
		} else if st, ok := w.SignalTypes.([]any); ok && len(st) > 0 {
			for _, x := range st {
				if str, ok := x.(string); ok {
					p.SignalTypes = append(p.SignalTypes, str)
				}
			}
		}
		if w.Kind == "account_group" {
			p.IdentityFilter = true
			p.LeadIds, p.Companies = accountIdentity(w.Config)
		} else if w.LeadID != nil {
			p.LeadID = pgtype.Int4{Int32: *w.LeadID, Valid: true}
		}
		if p.IdentityFilter && len(p.LeadIds) == 0 && len(p.Companies) == 0 {
			return nil // no identity to match: an empty page, cursor not decoded
		}
		if q.Cursor != "" {
			ts, cid, err := signals.DecodeCursor(q.Cursor)
			if err != nil {
				return &HTTPError{Status: 422, Detail: err.Error()}
			}
			p.CursorTs = pgtype.Float8{Float64: ts, Valid: true}
			p.CursorID = pgtype.Text{String: cid, Valid: true}
		} else {
			p.RowOffset = int32(min(q.Offset, math.MaxInt32))
			o := q.Offset
			pg.Offset = &o
		}
		rows, err := gen.New(tx).WatchSvcSignalsPage(ctx, p)
		if err != nil {
			return err
		}
		pg.HasMore = len(rows) > q.Limit
		if pg.HasMore {
			rows = rows[:q.Limit]
		}
		pg.Signals = make([]signals.Signal, len(rows))
		for i, r := range rows {
			pg.Signals[i] = sigFrom(r)
		}
		if pg.HasMore && len(pg.Signals) > 0 {
			last := pg.Signals[len(pg.Signals)-1]
			if last.CreatedAt != nil {
				c := signals.EncodeCursor(*last.CreatedAt, last.ID)
				pg.NextCursor = &c
			}
		}
		return nil
	})
	if pg.Signals == nil {
		pg.Signals = []signals.Signal{}
	}
	if pg.Offset == nil && q.Cursor == "" {
		o := q.Offset
		pg.Offset = &o
	}
	return pg, err
}

// accountIdentity expands an account_group watch's accounts into the sorted
// distinct lead ids and case-folded company names it matches signals on.
func accountIdentity(config any) ([]int32, []string) {
	leadSet := map[int32]bool{}
	compSet := map[string]bool{}
	accounts, _ := objGet(config, "accounts").([]any)
	fold := cases.Fold()
	for _, it := range accounts {
		o, ok := it.(*wbcrud.Obj)
		if !ok {
			continue
		}
		if lv, _ := o.Get("lead_id"); lv != nil {
			if n, ok := pyIntOf(lv); ok && n >= 0 && n <= 1<<31-1 {
				leadSet[int32(n)] = true
			}
		}
		if cv, _ := o.Get("company"); truthyStr(cv) {
			c := strings.TrimSpace(fold.String(strings.TrimSpace(fmt.Sprint(cv))))
			if c != "" {
				compSet[c] = true
			}
		}
	}
	leads := make([]int32, 0, len(leadSet))
	for n := range leadSet {
		leads = append(leads, n)
	}
	sort.Slice(leads, func(i, j int) bool { return leads[i] < leads[j] })
	comps := make([]string, 0, len(compSet))
	for c := range compSet {
		comps = append(comps, c)
	}
	sort.Strings(comps)
	return leads, comps
}
