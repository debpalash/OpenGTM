package ambitionimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourceingest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// JobType is the queue job type of this import.
const JobType = "ambitionbox_import"

// JobTimeout is JOB_TIMEOUTS["ambitionbox_import"].
const JobTimeout = 900 * time.Second

// ErrLeaseLost: the job lease was lost or the job cancelled; the page loop
// stopped before the next fetch.
var ErrLeaseLost = errors.New("ambitionbox_import: lease lost or job cancelled")

const (
	errTextLimit  = 1000
	settleTimeout = 30 * time.Second
	notFoundText  = "connector run or workbook not found"
)

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

type ids struct{ ws, wb, run string }

func payloadIDs(raw []byte) (ids, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ids{}, false
	}
	s := func(k string) string {
		v, ok := m[k]
		if !ok || v == nil || v == "" || v == false {
			return ""
		}
		if str, ok := v.(string); ok {
			return strings.TrimSpace(str)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	p := ids{ws: s("workspace_id"), wb: s("workbook_id"), run: s("run_id")}
	return p, p.ws != "" && p.wb != "" && p.run != ""
}

// parseNumberObject decodes a JSON object with numbers kept exact.
func parseObject(text string) map[string]any {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	m := map[string]any{}
	_ = dec.Decode(&m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func asInt(v any, def int) int {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		if f, err := x.Float64(); err == nil {
			return int(f)
		}
	case float64:
		return int(x)
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return i
		}
	case bool:
		if x {
			return 1
		}
	}
	return def
}

func asStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return string(x)
	}
	return fmt.Sprint(v)
}

// mergeConfig is `{**(source_config or {}), **updates}`: stored key order is
// kept, new keys are appended.
func mergeConfig(stored string, set func(o *wbcrud.Obj)) (string, error) {
	o := wbcrud.NewObj()
	if s := strings.TrimSpace(stored); s != "" && s != "null" {
		if parsed, err := wbcrud.ParseObject([]byte(s)); err == nil {
			o = parsed
		}
	}
	set(o)
	return marshal(o)
}

type step struct {
	finished bool
	page     int
	industry string
	rating   string
	sortBy   string
}

// Handle is handle_ambitionbox_import: resume the run from its committed page
// cursor, one transaction per page. A nil claim skips lease checks.
func Handle(ctx context.Context, d Deps, c *queue.Claim) error {
	var raw []byte
	if c != nil {
		raw = c.Payload
	}
	p, ok := payloadIDs(raw)
	if !ok {
		return errors.New("ambitionbox_import requires workspace_id, workbook_id, run_id")
	}
	err := pageLoop(ctx, d, c, p)
	if err != nil {
		// Keep the run active (retrying) until the queue decides retry vs final.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		defer cancel()
		if serr := settle(sctx, d, p, "retrying", err.Error()); serr != nil {
			d.log().Error("ambitionbox import: marking run retrying failed", "run_id", p.run, "err", serr)
		}
	}
	return err
}

func pageLoop(ctx context.Context, d Deps, c *queue.Claim, p ids) error {
	for {
		if c != nil {
			active, err := queue.Heartbeat(ctx, d.Pool, c)
			if err != nil {
				return err
			}
			if !active {
				return ErrLeaseLost
			}
			if cancelled, err := queue.IsCancelled(ctx, d.Pool, c); err != nil {
				return err
			} else if cancelled {
				return ErrLeaseLost
			}
		}
		st, err := markRunning(ctx, d, p)
		if err != nil {
			return err
		}
		if st.finished {
			return nil
		}
		pp := ambitionbox.PageParams{Page: st.page, SortBy: st.sortBy, Rating: st.rating}
		if st.industry != "" {
			pp.Industry = []string{st.industry}
		}
		// The fetch runs outside any transaction.
		page, err := d.Fetcher.FetchCompanyPage(ctx, pp)
		if err != nil {
			return err
		}
		done, added, total, advanced, err := commitPage(ctx, d, p, st.page, page)
		if err != nil {
			return err
		}
		if !advanced {
			continue // another worker committed this page first
		}
		if d.Progress != nil {
			d.Progress.Publish(ctx, p.wb, map[string]any{"type": "source_page", "connector": "ambitionbox", "runId": p.run,
				"page": st.page, "added": added, "total": total, "done": done})
		}
		if done {
			d.log().Info("ambitionbox import finished", "run_id", p.run, "rows", total)
			return nil
		}
	}
}

func notFound() error { return &queue.PermanentError{Err: errors.New(notFoundText)} }

// markRunning is the first block of the Python loop: mark the run and workbook
// running and read the cursor and query.
func markRunning(ctx context.Context, d Deps, p ids) (st step, err error) {
	err = tenancy.InWorkspace(ctx, d.Pool, p.ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		run, err := q.AILoadRun(ctx, gen.AILoadRunParams{RunID: p.run, WorkbookID: p.wb, WorkspaceID: p.ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound()
		}
		if err != nil {
			return err
		}
		wb, err := q.AILoadWorkbook(ctx, gen.AILoadWorkbookParams{WorkbookID: p.wb, WorkspaceID: p.ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound()
		}
		if err != nil {
			return err
		}
		if run.Status == "complete" || run.Status == "partial" {
			st.finished = true
			return nil
		}
		if err := q.AIMarkRunning(ctx, gen.AIMarkRunningParams{RunID: p.run, WorkspaceID: p.ws}); err != nil {
			return err
		}
		cfg, err := mergeConfig(wb.SourceConfig, func(o *wbcrud.Obj) {
			o.Set("run_status", "running")
			o.Set("last_error", nil)
		})
		if err != nil {
			return err
		}
		if err := q.AISetWorkbookState(ctx, gen.AISetWorkbookStateParams{Status: "running", SourceConfig: cfg, WorkbookID: p.wb, WorkspaceID: p.ws}); err != nil {
			return err
		}
		cur, query := parseObject(run.Cursor), parseObject(run.Query)
		st.page = asInt(cur["next_page"], 1)
		if st.page < 1 {
			st.page = 1
		}
		st.industry = asStr(query["industry"])
		st.rating = asStr(query["rating"])
		st.sortBy = asStr(query["sort_by"])
		if st.sortBy == "" {
			st.sortBy = "popular"
		}
		return nil
	})
	return st, err
}

// commitPage is the second block: one transaction that re-checks the cursor,
// upserts the page and advances cursor, counters, status and source_config.
func commitPage(ctx context.Context, d Deps, p ids, pageNumber int, page sourceingest.Page) (done bool, added, total int, advanced bool, err error) {
	err = tenancy.InWorkspace(ctx, d.Pool, p.ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		run, err := q.AILoadRun(ctx, gen.AILoadRunParams{RunID: p.run, WorkbookID: p.wb, WorkspaceID: p.ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("connector run or workbook disappeared")
		}
		if err != nil {
			return err
		}
		wb, err := q.AILoadWorkbook(ctx, gen.AILoadWorkbookParams{WorkbookID: p.wb, WorkspaceID: p.ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("connector run or workbook disappeared")
		}
		if err != nil {
			return err
		}
		cur, query := parseObject(run.Cursor), parseObject(run.Query)
		if asInt(cur["next_page"], 1) != pageNumber {
			return nil // another retry/process committed this page first
		}
		advanced = true
		resetDone, _ := cur["reset_done"].(bool)
		replace, _ := query["replace_existing"].(bool)
		reset := replace && pageNumber == 1 && !resetDone
		seen := map[string]bool{}
		if l, ok := cur["seen_record_ids"].([]any); ok {
			for _, v := range l {
				seen[asStr(v)] = true
			}
		}
		remaining := int(run.RequestedCount) - len(seen)
		if remaining < 0 {
			remaining = 0
		}
		var recs []sourceingest.Record
		if remaining > 0 {
			for _, r := range page.Records {
				if seen[r.RecordID] {
					continue
				}
				data := r.Data
				if data == nil {
					data = wbcrud.NewObj()
				}
				recs = append(recs, sourceingest.Record{Provider: "ambitionbox", RecordID: r.RecordID, Data: CompanyToRow(data),
					Rank: r.Rank, SourceURL: r.SourceURL, FetchedAt: r.FetchedAt})
				seen[r.RecordID] = true
				if len(recs) >= remaining {
					break
				}
			}
		}
		sum, err := sourceingest.UpsertNotify(ctx, tx, d.Hook, p.wb, p.ws, "ambitionbox", recs, reset)
		if err != nil {
			return err
		}
		counts, err := q.AICountRows(ctx, gen.AICountRowsParams{WorkbookID: p.wb, WorkspaceID: p.ws})
		if err != nil {
			return err
		}
		seenList := make([]string, 0, len(seen))
		for s := range seen {
			seenList = append(seenList, s)
		}
		sort.Strings(seenList)
		anySeen := make([]any, len(seenList))
		for i, s := range seenList {
			anySeen[i] = s
		}
		cursor := wbcrud.NewObj()
		cursor.Set("next_page", pageNumber+1)
		cursor.Set("reset_done", resetDone || reset)
		cursor.Set("seen_record_ids", anySeen)
		cs, err := marshal(cursor)
		if err != nil {
			return err
		}
		pages := int(run.PagesFetched) + 1
		fetched := len(seen)
		targetMet := fetched >= int(run.RequestedCount)
		exhausted := !page.HasMore
		maxPages := asInt(query["max_pages"], 25)
		if maxPages == 0 {
			maxPages = 25
		}
		maxReached := pages >= maxPages
		done = targetMet || exhausted || maxReached
		status := "running"
		var errText pgtype.Text
		wbStatus := wb.Status.String
		if done {
			status = "partial"
			if targetMet {
				status = "complete"
			}
			if maxReached && !targetMet && !exhausted {
				errText = pgtype.Text{String: "maximum page budget reached before requested target", Valid: true}
			}
			wbStatus = "draft"
		}
		var srcTotal pgtype.Int4
		if page.SourceTotal != nil {
			srcTotal = pgtype.Int4{Int32: int32(*page.SourceTotal), Valid: true}
		}
		if err := q.AIUpdateRunPage(ctx, gen.AIUpdateRunPageParams{Status: status, Cursor: cs, PagesFetched: int32(pages),
			FetchedCount: int32(fetched), AddedCount: run.AddedCount + int32(sum.Added), UpdatedCount: run.UpdatedCount + int32(sum.Updated),
			SkippedCount: run.SkippedCount + int32(sum.Skipped), SourceTotal: srcTotal, TargetMet: targetMet, Exhausted: exhausted,
			Error: errText, Done: done, RunID: p.run, WorkspaceID: p.ws}); err != nil {
			return err
		}
		cfg, err := mergeConfig(wb.SourceConfig, func(o *wbcrud.Obj) {
			o.Set("returned", int(counts.Ambitionbox))
			o.Set("pages_fetched", pages)
			if page.SourceTotal != nil {
				o.Set("source_total", *page.SourceTotal)
			} else {
				o.Set("source_total", nil)
			}
			o.Set("target_met", targetMet)
			o.Set("run_status", status)
		})
		if err != nil {
			return err
		}
		if err := q.AISetWorkbookState(ctx, gen.AISetWorkbookStateParams{Status: wbStatus, SourceConfig: cfg,
			TotalRows: pgtype.Int4{Int32: counts.Total, Valid: true}, WorkbookID: p.wb, WorkspaceID: p.ws}); err != nil {
			return err
		}
		added, total = len(sum.AddedRowIDs), fetched
		return nil
	})
	if err != nil {
		return false, 0, 0, false, err
	}
	return done, added, total, advanced, nil
}

// settle makes run and workbook agree with a failed attempt: retrying keeps
// both active, failed ends them. A finished run is never touched.
func settle(ctx context.Context, d Deps, p ids, status, reason string) error {
	reason = truncate(reason, errTextLimit)
	return tenancy.InWorkspace(ctx, d.Pool, p.ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		run, err := q.AILoadRun(ctx, gen.AILoadRunParams{RunID: p.run, WorkbookID: p.wb, WorkspaceID: p.ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		wb, err := q.AILoadWorkbook(ctx, gen.AILoadWorkbookParams{WorkbookID: p.wb, WorkspaceID: p.ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if run.Status == "complete" || run.Status == "partial" {
			return nil
		}
		terminal := status != "retrying"
		if _, err := q.AISetRunOutcome(ctx, gen.AISetRunOutcomeParams{Status: status, Error: reason, Terminal: terminal,
			RunID: p.run, WorkbookID: p.wb, WorkspaceID: p.ws}); err != nil {
			return err
		}
		cfg, err := mergeConfig(wb.SourceConfig, func(o *wbcrud.Obj) {
			o.Set("run_status", status)
			o.Set("last_error", reason)
			o.Set("target_met", run.TargetMet)
		})
		if err != nil {
			return err
		}
		wbStatus := "draft"
		if !terminal {
			wbStatus = "running"
		}
		return q.AISetWorkbookState(ctx, gen.AISetWorkbookStateParams{Status: wbStatus, SourceConfig: cfg, WorkbookID: p.wb, WorkspaceID: p.ws})
	})
}
