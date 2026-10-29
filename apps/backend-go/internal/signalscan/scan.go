package signalscan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadswrite"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// hiringProvider is the Python provider that implements the hiring detector
// (JobSpySignalProvider: registry name "jobspy", module jobspy_signals). It
// is served through the provider-call seam (provcall), not in process.
const hiringProvider = "jobspy"

// Caller runs provider calls (provcall.Caller: the in-process rpclocal caller).
type Caller interface {
	Call(ctx context.Context, call provcall.Call) (provcall.Result, error)
}

// Caps are the SIGNAL_SCAN_* settings.
type Caps struct {
	HotLeads, WarmLeads, PerWorkspace, Global int
}

// DefaultCaps mirrors the Python settings defaults (50/30/20/500).
func DefaultCaps() Caps { return Caps{HotLeads: 50, WarmLeads: 30, PerWorkspace: 20, Global: 500} }

// Deps is everything the scan needs.
type Deps struct {
	Pool *pgxpool.Pool
	// Py runs the hiring detector. nil: no detector is available and a scan finds no signals
	// (Python: "JobSpy provider not available").
	Py   Caller
	Hook automationhook.Hook
	Log  *slog.Logger
	Now  func() time.Time
	// Caps nil means DefaultCaps. A Global cap of 0 means unlimited (Python).
	Caps          *Caps
	HiringTimeout time.Duration // per-lead provider call limit; default 60s
	// RefreshExecutor / ScanExecutor choose the executor of enqueued refresh_workbook and
	// signal_scan jobs; nil: "go", the only executor since the plan 15 cutover.
	RefreshExecutor func(workspaceID string) string
	ScanExecutor    func() string
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) caps() Caps {
	if d.Caps != nil {
		return *d.Caps
	}
	return DefaultCaps()
}

func (d Deps) timeout() time.Duration {
	if d.HiringTimeout > 0 {
		return d.HiringTimeout
	}
	return 60 * time.Second
}

// Counts is run_signal_scan's result.
type Counts struct {
	Scanned, SignalsFound, LeadsBoosted, Workspaces, WorkspacesFailed int
	// SignalsInserted counts the rows Go actually inserted (not already present).
	SignalsInserted int
}

// Lead is the slice of a lead the scan needs. Input is the full lead JSON sent to the
// Python provider (empty: {"id","company"}).
type Lead struct {
	ID      int64
	Company string
	Score   *int64
	Input   json.RawMessage
}

// ExtractHiringData is monitor._extract_hiring_data over a provider-call result: the
// "hiring_signals" field (a JSON string, or an object) of a successful result, or nil.
func ExtractHiringData(res provcall.Result) map[string]any {
	if res.Status != "ok" || len(res.Output) == 0 {
		return nil
	}
	var out struct {
		Success bool `json:"success"`
		Fields  struct {
			HiringSignals json.RawMessage `json:"hiring_signals"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil || !out.Success {
		return nil
	}
	raw := bytes.TrimSpace(out.Fields.HiringSignals)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return nil
		}
		raw = []byte(s)
	} else if raw[0] != '{' {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil || dec.More() {
		return nil
	}
	if len(m) == 0 {
		return nil // empty dict is falsy in Python
	}
	return m
}

// totalJobs reads a positive integer total_jobs (anything else is skipped: Python
// would also skip missing/zero/negative; non-integer JSON numbers are not supported).
func totalJobs(m map[string]any) (int64, bool) {
	n, ok := m["total_jobs"].(json.Number)
	if !ok {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// topRoles is ', '.join(jobs_data.get('top_titles', [])[:3]); ok=false when Python would raise.
func topRoles(m map[string]any) (string, bool) {
	v, present := m["top_titles"]
	if !present {
		return "", true
	}
	switch t := v.(type) {
	case []any:
		if len(t) > 3 {
			t = t[:3]
		}
		parts := make([]string, len(t))
		for i, e := range t {
			s, ok := e.(string)
			if !ok {
				return "", false
			}
			parts[i] = s
		}
		return strings.Join(parts, ", "), true
	case string: // a str slices into characters
		rs := []rune(t)
		if len(rs) > 3 {
			rs = rs[:3]
		}
		parts := make([]string, len(rs))
		for i, r := range rs {
			parts[i] = string(r)
		}
		return strings.Join(parts, ", "), true
	}
	return "", false
}

// detectHiring is monitor.detect_hiring_signals: for the first cap leads with a
// company, ask the Python hiring provider; one lead's failure never affects another.
func detectHiring(ctx context.Context, d Deps, workspaceID string, leads []Lead, cap int) []Detected {
	if d.Py == nil {
		d.log().Warn("signal scan: hiring provider unavailable (no provider-call client)")
		return nil
	}
	if cap < 0 {
		cap = 0
	}
	if cap < len(leads) {
		leads = leads[:cap]
	}
	now := d.now()
	run := fmt.Sprintf("signal_scan:%d", now.UnixNano()) // one scan: its calls share a ledger scope
	var out []Detected
	for i, lead := range leads {
		if lead.Company == "" {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		input := lead.Input
		if len(input) == 0 {
			input, _ = json.Marshal(map[string]any{"id": lead.ID, "company": lead.Company})
		}
		cctx := provcall.WithSpendScope(ctx, provcall.SpendScope{Run: run, Row: fmt.Sprintf("lead:%d", lead.ID)})
		res, err := d.Py.Call(cctx, provcall.Call{
			CallID:      fmt.Sprintf("signal_scan:%s:%d:%d:%d", workspaceID, lead.ID, now.UnixNano(), i),
			Provider:    hiringProvider,
			WorkspaceID: workspaceID,
			Input:       input,
			Timeout:     d.timeout(),
		})
		if err != nil {
			d.log().Debug("hiring check failed", "workspace_id", workspaceID, "lead_id", lead.ID)
			continue
		}
		jobs := ExtractHiringData(res)
		if jobs == nil {
			continue
		}
		total, ok := totalJobs(jobs)
		if !ok {
			continue
		}
		roles, ok := topRoles(jobs)
		if !ok {
			continue
		}
		weight := total
		if weight > 10 {
			weight = 10
		}
		out = append(out, Detected{
			ID:          ScanSignalID(workspaceID, lead.ID, "hiring", HiringBand(total), now),
			WorkspaceID: workspaceID, LeadID: lead.ID, Company: lead.Company, SignalType: "hiring",
			Title:       fmt.Sprintf("%s has %d open positions", lead.Company, total),
			Description: "Active hiring detected. Top roles: " + roles,
			Source:      "JobSpy", Weight: int(weight),
		})
	}
	return out
}

// Run is monitor.run_signal_scan: per workspace (each under tenancy.InWorkspace), the hot
// then warm leads are checked for hiring activity, new signals are inserted
// (ON CONFLICT (id) DO NOTHING) and on_signal automations fire for the rows actually
// inserted, then scores are boosted. One workspace failing is logged and skipped.
func Run(ctx context.Context, d Deps) (Counts, error) {
	var c Counts
	caps := d.caps()
	wsIDs, err := gen.New(d.Pool).ScanListWorkspaceIDs(ctx)
	if err != nil {
		d.log().Warn("signal scan: could not list workspaces", "err", err.Error())
		return c, nil
	}
	budget, limited := caps.Global, caps.Global > 0
	for _, ws := range wsIDs {
		if ctx.Err() != nil {
			break
		}
		if limited && budget <= 0 {
			d.log().Info("signal scan: global lead cap reached, stopping workspace loop")
			break
		}
		leads, err := d.hotWarmLeads(ctx, ws, caps)
		if err != nil {
			c.WorkspacesFailed++
			d.log().Warn("signal scan failed for workspace", "workspace_id", ws, "err", err.Error())
			continue
		}
		if len(leads) == 0 {
			c.Workspaces++
			continue
		}
		cap := caps.PerWorkspace
		if limited && budget < cap {
			cap = budget
		}
		if cap <= 0 {
			break
		}
		signals := detectHiring(ctx, d, ws, leads, cap)
		inserted, err := d.store(ctx, ws, signals)
		if err != nil {
			c.WorkspacesFailed++
			d.log().Warn("signal scan failed for workspace", "workspace_id", ws, "err", err.Error())
			continue
		}
		boosted := d.applyBoosts(ctx, ws, signals)
		processed := len(leads)
		if cap < processed {
			processed = cap
		}
		c.Scanned += len(leads)
		c.SignalsFound += len(signals)
		c.SignalsInserted += inserted
		c.LeadsBoosted += boosted
		if limited {
			budget -= processed
		}
		c.Workspaces++
	}
	d.log().Info("signal scan complete", "workspaces", c.Workspaces, "failed", c.WorkspacesFailed,
		"leads", c.Scanned, "signals", c.SignalsFound, "inserted", c.SignalsInserted, "boosted", c.LeadsBoosted)
	return c, nil
}

func (d Deps) hotWarmLeads(ctx context.Context, ws string, caps Caps) ([]Lead, error) {
	var out []Lead
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		for _, t := range []struct {
			tier  string
			limit int
		}{{"hot", caps.HotLeads}, {"warm", caps.WarmLeads}} {
			rows, err := leadsread.ListLeads(ctx, tx, leadsread.ListParams{ScoreTier: t.tier, Limit: t.limit})
			if err != nil {
				return err
			}
			for _, r := range rows {
				in, _ := json.Marshal(r)
				out = append(out, Lead{ID: r.ID, Company: r.Company, Score: r.Score, Input: in})
			}
		}
		return nil
	})
	return out, err
}

// store is SignalStore.add_signal per signal, each in its own transaction (like Python):
// insert-only-when-absent, and on_signal is emitted only for a row actually inserted, in
// the same transaction. Returns the number of rows inserted.
func (d Deps) store(ctx context.Context, ws string, signals []Detected) (int, error) {
	inserted := 0
	created := float64(d.now().UnixNano()) / 1e9
	for _, s := range signals {
		err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
			_, err := gen.New(tx).ScanInsertSignal(ctx, gen.ScanInsertSignalParams{
				ID: s.ID, WorkspaceID: ws, LeadID: int32(s.LeadID), Company: s.Company, SignalType: s.SignalType,
				Title: s.Title, Description: s.Description, Source: s.Source, SourceUrl: s.SourceURL,
				Weight: int32(s.Weight), CreatedAt: created,
			})
			if err == pgx.ErrNoRows {
				return nil // already present (a re-scan, or a racing Python scan): no emit
			}
			if err != nil {
				return err
			}
			inserted++
			if d.Hook != nil {
				lead := s.LeadID
				_, _ = d.Hook.SignalsInserted(ctx, tx, ws, []automationhook.SignalRef{{PK: s.ID, Type: s.SignalType, LeadID: &lead}})
			}
			return nil
		})
		if err != nil {
			return inserted, err
		}
	}
	return inserted, nil
}

// applyBoosts is monitor._apply_signal_boosts: failures are logged, never raised.
func (d Deps) applyBoosts(ctx context.Context, ws string, signals []Detected) int {
	if len(signals) == 0 {
		return 0
	}
	boosted := 0
	err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		var lookupErr error
		ups := PlanBoosts(signals, func(id int64) (*int64, bool) {
			sc, err := q.ScanLeadScore(ctx, int32(id))
			if err == pgx.ErrNoRows {
				return nil, false
			}
			if err != nil {
				lookupErr = err
				return nil, false
			}
			if !sc.Valid {
				return nil, true
			}
			v := int64(sc.Int32)
			return &v, true
		})
		if lookupErr != nil {
			return lookupErr
		}
		for _, u := range ups {
			if _, err := leadswrite.UpdateFields(ctx, tx, u.LeadID, map[string]any{"score": u.Score, "score_tier": u.Tier}); err != nil {
				return err
			}
			boosted++
		}
		return nil
	})
	if err != nil {
		d.log().Warn("signal score boost failed", "workspace_id", ws, "err", err.Error())
		return 0
	}
	return boosted
}
