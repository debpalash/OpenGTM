// Package destsync is the audience destination sync worker (engine.py's
// handle_destination_sync): one queued run streams the audience's members to a
// destination, keeps the delivery ledger, honours cooperative cancellation at the exact
// points Python checks it, and settles the run and the destination's health.
//
// Transactions: every step is its own short tenancy.InWorkspace transaction (a member page,
// one member's cancel-check and ledger upsert, one settle). Nothing external (Deliverer,
// SyncBatch, warehouse Sync) is ever called while a transaction is open, so no connection or
// row lock is held across a provider round trip. Workspace-scoped statements all run inside
// the tenant the job names (validated against the job row's own workspace).
package destsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/adsdrv"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/warehouse"
)

// Python's engine constants; Deps may override them (the transcripts run with small values).
const (
	DefaultMemberPageSize = 500
	DefaultAdBatchSize    = 5000
	defaultLeaseEvery     = 2 * time.Second
)

// ErrLeaseLost: the job's lease was lost or the job cancelled; the run stops before the next
// external call and writes nothing more (the queue's retry and the failure reconcile own the state).
var ErrLeaseLost = errors.New("destination sync: lease lost or job cancelled")

// Deliverer delivers one member to a one-by-one destination (destdeliver.Deliverer).
type Deliverer interface {
	Deliver(ctx context.Context, dest destdeliver.Destination, leadID int, snapshot map[string]any, idem string) destdeliver.Result
}

// AdSyncer uploads one bounded ad-platform batch (adsdrv.SyncBatch).
type AdSyncer interface {
	SyncBatch(ctx context.Context, wsID, destType string, cfg map[string]any, snapshots []map[string]any, op string) (adsdrv.Result, error)
}

// WarehouseSyncer posts the buffered export (warehouse.Sync).
type WarehouseSyncer interface {
	Sync(ctx context.Context, wsID string, dest warehouse.DestRef, b *warehouse.Buffer) (warehouse.Result, error)
}

type adSyncer struct{ c adsdrv.Client }

// NewAdSyncer adapts an adsdrv.Client.
func NewAdSyncer(c adsdrv.Client) AdSyncer { return adSyncer{c} }

func (a adSyncer) SyncBatch(ctx context.Context, wsID, destType string, cfg map[string]any, snaps []map[string]any, op string) (adsdrv.Result, error) {
	return adsdrv.SyncBatch(ctx, a.c, wsID, destType, cfg, snaps, op)
}

type whSyncer struct {
	c   warehouse.Client
	get warehouse.SecretGetter
}

// NewWarehouseSyncer adapts a warehouse.Client and its workspace-secret getter.
func NewWarehouseSyncer(c warehouse.Client, get warehouse.SecretGetter) WarehouseSyncer {
	return whSyncer{c, get}
}

func (w whSyncer) Sync(ctx context.Context, wsID string, dest warehouse.DestRef, b *warehouse.Buffer) (warehouse.Result, error) {
	return warehouse.Sync(ctx, w.c, wsID, dest, b, w.get)
}

// Deps are the engine's collaborators.
type Deps struct {
	Pool      *pgxpool.Pool
	Deliverer Deliverer
	Ads       AdSyncer
	Warehouse WarehouseSyncer

	MemberPageSize int // 0 = 500
	AdBatchSize    int // 0 = 5000
	// WarehouseMaxMemory bounds the export buffer before it spills to an unlinked temp file (0 = default).
	WarehouseMaxMemory int
	// LeaseEvery throttles the lease check made before each external step: 0 = every 2s,
	// negative = before every step.
	LeaseEvery time.Duration
	// Lease reports whether the claim is still ours and not cancelled (default: queue.Heartbeat
	// then queue.IsCancelled on Pool).
	Lease func(ctx context.Context, c *queue.Claim) (active bool, err error)
	Log   *slog.Logger
}

// Engine runs destination sync jobs. It holds no per-run state and is safe for concurrent Handle calls.
type Engine struct {
	d Deps

	// test seams: checkHook runs before the n-th (0-based) cancellation check of a run;
	// checks is the number of checks the most recent Handle made.
	checkHook func(n int)
	// raceHook runs inside a transaction between a read and the guarded UPDATE that follows it
	// ("before_start", "before_complete"); tests use it to land a competing write there.
	raceHook func(point string)
	checks   atomic.Int64
}

// New builds an Engine.
func New(d Deps) *Engine {
	if d.MemberPageSize <= 0 {
		d.MemberPageSize = DefaultMemberPageSize
	}
	if d.AdBatchSize <= 0 {
		d.AdBatchSize = DefaultAdBatchSize
	}
	if d.LeaseEvery == 0 {
		d.LeaseEvery = defaultLeaseEvery
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Engine{d: d}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// counters are the run's in-memory totals (Python's stats dict).
type counters struct{ attempted, succeeded, failed, skipped int }

// runState is one Handle call's working set.
type runState struct {
	c        *queue.Claim
	ws       string
	runID    string
	dest     deststore.Destination
	cfg      map[string]any
	fieldMap map[string]any
	dd       destdeliver.Destination
	ref      warehouse.DestRef
	ads      bool // meta_ads | google_ads | linkedin_ads
	wh       bool // warehouse_http
	st       counters
	checks   int
	lease    time.Time
}

func isAdType(t string) bool { return t == "meta_ads" || t == "google_ads" || t == "linkedin_ads" }

func (e *Engine) inTx(ctx context.Context, rs *runState, fn func(tx pgx.Tx) error) error {
	return tenancy.InWorkspace(ctx, e.d.Pool, rs.ws, fn)
}

// decodeObj decodes a stored JSON object with exact numbers; anything else is an empty object.
func decodeObj(raw []byte) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func decodeSnapshot(raw []byte) (map[string]any, error) {
	if t := bytes.TrimSpace(raw); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return map[string]any{}, nil
	}
	m, err := decodeObj(raw)
	if err != nil {
		return nil, fmt.Errorf("destsync: member snapshot: %w", err)
	}
	if bytes.TrimSpace(raw)[0] != '{' {
		return nil, errors.New("destsync: member snapshot is not an object")
	}
	return m, nil
}

// guard stops the run when the lease is gone; throttled by LeaseEvery.
func (e *Engine) guard(ctx context.Context, rs *runState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.d.LeaseEvery > 0 && !rs.lease.IsZero() && time.Since(rs.lease) < e.d.LeaseEvery {
		return nil
	}
	rs.lease = time.Now()
	var active bool
	var err error
	if e.d.Lease != nil {
		active, err = e.d.Lease(ctx, rs.c)
	} else {
		active, err = queue.Heartbeat(ctx, e.d.Pool, rs.c)
		if err == nil && active {
			var cancelled bool
			if cancelled, err = queue.IsCancelled(ctx, e.d.Pool, rs.c); err == nil && cancelled {
				active = false
			}
		}
	}
	if err != nil {
		return err
	}
	if !active {
		return ErrLeaseLost
	}
	return nil
}

// Handle is handle_destination_sync.
func (e *Engine) Handle(ctx context.Context, c *queue.Claim) error {
	if c == nil {
		return &queue.PermanentError{Err: errors.New(errSyncPayload)}
	}
	ws, runID, err := syncPayload(c.Payload, c.WorkspaceID)
	if err != nil {
		return err
	}
	rs := &runState{c: c, ws: ws, runID: runID}
	defer func() { e.checks.Store(int64(rs.checks)) }()

	proceed, err := e.begin(ctx, rs)
	if err != nil || !proceed {
		return err
	}
	var buf *warehouse.Buffer
	if rs.wh {
		buf = warehouse.NewBuffer(rs.runID, rs.ws, rs.ref, e.d.WarehouseMaxMemory)
		defer buf.Close()
	}
	return e.stream(ctx, rs, buf)
}

// begin reads the run and decides what to do with it (the head of handle_destination_sync):
// unknown, foreign and terminal runs are left alone, a cancelling run finishes cancelled, a
// missing or disabled destination cancels the run, anything else starts running.
func (e *Engine) begin(ctx context.Context, rs *runState) (proceed bool, err error) {
	err = e.inTx(ctx, rs, func(tx pgx.Tx) error {
		run, err := deststore.GetRun(ctx, tx, rs.runID)
		if errors.Is(err, deststore.ErrRunNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if run.WorkspaceID != rs.ws { // cannot happen under RLS + the workspace filter; fail closed
			return ErrWorkspaceMismatch
		}
		switch run.Status {
		case "completed", "completed_with_errors", "failed", "cancelled":
			return nil
		}
		dest, err := deststore.GetDestination(ctx, tx, run.DestinationID)
		var destPtr *deststore.Destination
		switch {
		case err == nil:
			destPtr = &dest
		case errors.Is(err, deststore.ErrNotFound):
		default:
			return err
		}
		if run.Status == "cancelling" {
			_, err := deststore.FinishCancelled(ctx, tx, run, destPtr)
			return err
		}
		q := gen.New(tx)
		if destPtr == nil || !destPtr.Enabled {
			return q.DSRunCancelUnusable(ctx, gen.DSRunCancelUnusableParams{WorkspaceID: rs.ws, ID: rs.runID})
		}
		if rs.cfg, err = decodeObj(dest.Config); err != nil {
			return fmt.Errorf("destsync: destination config: %w", err)
		}
		if rs.fieldMap, err = decodeObj(dest.FieldMap); err != nil {
			return fmt.Errorf("destsync: destination field map: %w", err)
		}
		e.race("before_start")
		if _, err := q.DSRunStart(ctx, gen.DSRunStartParams{WorkspaceID: rs.ws, ID: rs.runID}); errors.Is(err, pgx.ErrNoRows) {
			// the run left pending/running after the read above: a cancel (or a reconcile) won
			return e.yieldToNewState(ctx, tx, rs, destPtr)
		} else if err != nil {
			return fmt.Errorf("destsync: start run: %w", err)
		}
		rs.dest = dest
		proceed = true
		return nil
	})
	if err != nil || !proceed {
		return false, err
	}
	d := rs.dest
	rs.dd = destdeliver.Destination{ID: d.ID, WorkspaceID: d.WorkspaceID, AudienceID: d.AudienceID, Type: d.Type, Config: rs.cfg, FieldMap: rs.fieldMap}
	rs.ref = warehouse.DestRef{ID: d.ID, AudienceID: d.AudienceID, Config: rs.cfg}
	rs.ads, rs.wh = isAdType(d.Type), d.Type == "warehouse_http"
	return true, nil
}

type member struct {
	id, leadID int32
	snapshot   []byte
}

func (e *Engine) memberPage(ctx context.Context, rs *runState, after int32) ([]member, error) {
	var out []member
	err := e.inTx(ctx, rs, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).DSMemberPage(ctx, gen.DSMemberPageParams{
			WorkspaceID: rs.ws, AudienceID: rs.dest.AudienceID, AfterID: after, PageLimit: int32(e.d.MemberPageSize),
		})
		if err != nil {
			return fmt.Errorf("destsync: member page: %w", err)
		}
		out = out[:0]
		for _, r := range rows {
			out = append(out, member{r.ID, r.LeadID, r.Snapshot})
		}
		return nil
	})
	return out, err
}

// stream is the member loop and everything after it.
func (e *Engine) stream(ctx context.Context, rs *runState, buf *warehouse.Buffer) error {
	var adAdd []adCand
	memberCount := 0
	var after int32
	for {
		page, err := e.memberPage(ctx, rs, after)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			memberCount++
			cand, stop, err := e.oneMember(ctx, rs, m, buf)
			if err != nil || stop {
				return err
			}
			if cand != nil {
				adAdd = append(adAdd, *cand)
				if len(adAdd) >= e.d.AdBatchSize {
					if stop, err := e.flushAds(ctx, rs, "add", adAdd); err != nil || stop {
						return err
					}
					adAdd = adAdd[:0]
				}
			}
		}
		after = page[len(page)-1].id
	}
	if len(adAdd) > 0 {
		if stop, err := e.flushAds(ctx, rs, "add", adAdd); err != nil || stop {
			return err
		}
	}
	if rs.ads {
		if stop, err := e.reconcileExits(ctx, rs); err != nil || stop {
			return err
		}
	}
	if rs.wh {
		if stop, err := e.flushWarehouse(ctx, rs, buf); err != nil || stop {
			return err
		}
	}
	return e.finish(ctx, rs, memberCount)
}

type adCand struct {
	id       int32
	snapshot map[string]any
}

// oneMember is one pass of Python's member loop body. It returns an ad candidate to add to the
// pending batch, or stop=true when the run ended (cancelled).
func (e *Engine) oneMember(ctx context.Context, rs *runState, m member, buf *warehouse.Buffer) (cand *adCand, stop bool, err error) {
	if err := e.guard(ctx, rs); err != nil {
		return nil, true, err
	}
	snapshot, err := decodeSnapshot(m.snapshot)
	if err != nil {
		return nil, true, err
	}
	mapped := destdeliver.MapPayload(snapshot, rs.fieldMap)
	var hashed map[string]string
	if rs.ads {
		hashed = adsdrv.HashedIdentifiers(snapshot)
		mapped = make(map[string]any, len(hashed))
		for k, v := range hashed {
			mapped[k] = v
		}
	}
	fp := destdeliver.Fingerprint(mapped)
	idem := destdeliver.IdempotencyKey(rs.dest.ID, int(m.leadID), fp, false)

	in := deststore.UpsertDeliveryInput{
		RunID: rs.runID, DestinationID: rs.dest.ID, LeadID: int(m.leadID), Operation: "upsert", IdempotencyKey: idem,
		Fingerprint: fp, Payload: json.RawMessage(`{}`), Attempts: 0,
	}
	unsupported := rs.ads && !adsdrv.IdentifiersSupported(rs.dest.Type, hashed)
	deferred := rs.ads || rs.wh
	switch {
	case unsupported:
		msg := "No valid email or phone identifier"
		in.Status, in.Summary = "skipped", &msg
	case deferred:
		in.Status = "pending"
		if rs.ads {
			b, _ := json.Marshal(hashed)
			in.Payload = b
		}
	default:
		in.Status, in.Attempts = "in_flight", 1
	}

	var del deststore.Delivery
	var outcome deststore.UpsertOutcome
	var cancelled bool
	e.tick(rs)
	err = e.inTx(ctx, rs, func(tx pgx.Tx) error {
		c, err := e.checkInTx(ctx, tx, rs)
		if err != nil || c {
			cancelled = c
			return err
		}
		del, outcome, err = deststore.UpsertDelivery(ctx, tx, in)
		return err
	})
	if err != nil || cancelled {
		return nil, true, err
	}
	if outcome == deststore.AlreadySucceeded || unsupported {
		rs.st.skipped++
		return nil, false, nil
	}
	switch {
	case rs.ads:
		return &adCand{id: int32(del.ID), snapshot: snapshot}, false, nil
	case rs.wh:
		if err := buf.Append(int(m.leadID), mapped); err != nil {
			return nil, true, fmt.Errorf("destsync: warehouse buffer: %w", err)
		}
		return nil, false, nil
	}
	rs.st.attempted++
	res := e.deliverOne(ctx, rs, int(m.leadID), snapshot, idem)
	return nil, false, e.settleOne(ctx, rs, del.ID, res)
}

// deliverOne runs outside any transaction. A panic in a port becomes a failed delivery with a
// fixed text (a recovered value could carry a secret).
func (e *Engine) deliverOne(ctx context.Context, rs *runState, leadID int, snapshot map[string]any, idem string) (res destdeliver.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = destdeliver.Result{Error: "delivery failed unexpectedly"}
		}
	}()
	return e.d.Deliverer.Deliver(ctx, rs.dd, leadID, snapshot, idem)
}

func (e *Engine) settleOne(ctx context.Context, rs *runState, id int, res destdeliver.Result) error {
	status := "failed"
	if res.Success {
		status = "success"
	}
	in := deststore.SettleInput{Status: status, Summary: &res.Summary, Error: cleanError(res.Error), Delivered: res.Success}
	if res.ExternalID != "" {
		ext := res.ExternalID
		in.ExternalID = &ext
	}
	// Detached: the provider call is done, so its result is recorded even when the job context
	// was cancelled right after it (otherwise the retry would deliver the member again).
	sctx, cancel := settleContext(ctx)
	defer cancel()
	p := gen.DSDeliverySettleOneParams{
		WorkspaceID: rs.ws, ID: int32(id), RunID: rs.runID, Status: status,
		Summary: pgtype.Text{String: res.Summary, Valid: true}, Delivered: res.Success,
	}
	if in.Error != "" {
		p.Error = pgtype.Text{String: in.Error, Valid: true}
	}
	if in.ExternalID != nil {
		p.ExternalID = pgtype.Text{String: *in.ExternalID, Valid: true}
	}
	var n int64
	err := e.inTx(sctx, rs, func(tx pgx.Tx) error {
		var err error
		n, err = gen.New(tx).DSDeliverySettleOne(sctx, p)
		return err
	})
	if err != nil {
		return err
	}
	if n == 0 { // the row was closed or re-homed while the request was out: not ours to count
		return nil
	}
	if res.Success {
		rs.st.succeeded++
	} else {
		rs.st.failed++
	}
	return nil
}

// finish is the tail of handle_destination_sync: a last cancellation check (a cancel may have
// arrived while the final external request was in flight; completed results are kept), then
// run counters, terminal status and the destination's health in one transaction.
func (e *Engine) finish(ctx context.Context, rs *runState, memberCount int) error {
	e.tick(rs)
	return e.inTx(ctx, rs, func(tx pgx.Tx) error {
		if cancelled, err := e.checkInTx(ctx, tx, rs); err != nil || cancelled {
			return err
		}
		st := rs.st
		status, health := "completed", "healthy"
		var lastErr *string
		if st.failed > 0 {
			status, health = "completed_with_errors", "degraded"
			msg := fmt.Sprintf("%d deliveries failed", st.failed)
			lastErr = &msg
		}
		q := gen.New(tx)
		e.race("before_complete")
		finished, err := q.DSRunComplete(ctx, gen.DSRunCompleteParams{
			WorkspaceID: rs.ws, ID: rs.runID, Status: status,
			Attempted: int32(st.attempted), Succeeded: int32(st.succeeded), Failed: int32(st.failed), Skipped: int32(st.skipped),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// no longer running: a cancel request arrived (finish it) or the run was closed elsewhere (leave it)
			return e.yieldToNewState(ctx, tx, rs, &rs.dest)
		}
		if err != nil {
			return fmt.Errorf("destsync: complete run: %w", err)
		}
		p := gen.DSDestSetHealthParams{
			WorkspaceID: rs.ws, ID: rs.dest.ID, HealthStatus: health, FinishedAt: finished,
			SetSuccess: st.succeeded > 0 || (memberCount == 0 && st.failed == 0),
		}
		if lastErr != nil {
			p.LastError.String, p.LastError.Valid = *lastErr, true
		}
		if err := q.DSDestSetHealth(ctx, p); err != nil {
			return fmt.Errorf("destsync: destination health: %w", err)
		}
		e.d.Log.Info("destination sync finished", "run_id", rs.runID, "status", status,
			"attempted", st.attempted, "succeeded", st.succeeded, "failed", st.failed, "skipped", st.skipped)
		return nil
	})
}

// ErrWorkspaceMismatch: the job payload names a workspace other than the job row's own.
// Nothing is read or written for such a job.
var ErrWorkspaceMismatch = errors.New("destination job payload workspace does not match the job")

const (
	errSyncPayload    = "destination sync requires workspace_id and run_id"
	errFailurePayload = "destination failure payload requires workspace_id and run_id"
	maxErrorChars     = 1000
)

func payloadIDs(raw []byte) (ws, run string, ok bool) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil || m == nil {
		return "", "", false
	}
	ws, _ = m["workspace_id"].(string)
	run, _ = m["run_id"].(string)
	return ws, run, true
}

// syncPayload is handle_destination_sync's payload guard: workspace_id and run_id must be
// non-empty strings. A job row that carries a workspace column must agree with the payload,
// otherwise the job is refused (the payload is untrusted input; the run row is only ever
// looked up inside the tenant it names, and that tenant must be the job's own).
func syncPayload(raw []byte, claimWS string) (ws, run string, err error) {
	ws, run, ok := payloadIDs(raw)
	if !ok || ws == "" || run == "" {
		return "", "", &queue.PermanentError{Err: errors.New(errSyncPayload)}
	}
	if claimWS != "" && claimWS != ws {
		return "", "", &queue.PermanentError{Err: ErrWorkspaceMismatch}
	}
	return ws, run, nil
}

var (
	reAuthHeader = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:(?:bearer|basic|token)\s+)?[^\s"',}]+`)
	reBearer     = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]+`)
	reBasic      = regexp.MustCompile(`(?i)\b(basic)\s+([A-Za-z0-9._~+/=-]{8,})`)
	reUserinfo   = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)
	reKeyValue   = regexp.MustCompile(`(?i)\b(access_token|refresh_token|api[_-]?key|client_secret|secret|password|token|signature)=[^&\s"']+`)
	reJSONKey    = regexp.MustCompile(`(?i)("(?:access_token|refresh_token|api[_-]?key|client_secret|secret|password|token)"\s*:\s*")[^"]*"`)
	reLowerWord  = regexp.MustCompile(`^[a-z]+$`)
)

// redactSecrets removes credential shapes from text that came from a provider or a lower
// layer. The delivery layers already scrub the secret values they resolved; this is the
// engine's own backstop before anything is written to the ledger or a log.
func redactSecrets(s string) string {
	s = reAuthHeader.ReplaceAllString(s, "${1}[redacted]")
	s = reBearer.ReplaceAllString(s, "${1} [redacted]")
	s = reBasic.ReplaceAllStringFunc(s, func(m string) string {
		sub := reBasic.FindStringSubmatch(m)
		if reLowerWord.MatchString(sub[2]) { // ordinary prose: "basic authentication"
			return m
		}
		return sub[1] + " [redacted]"
	})
	s = reUserinfo.ReplaceAllString(s, "${1}[redacted]@")
	s = reKeyValue.ReplaceAllString(s, "${1}=[redacted]")
	s = reJSONKey.ReplaceAllString(s, `${1}[redacted]"`)
	return s
}

// cleanError is the text stored in a ledger row: credentials removed first, then cut to
// 1000 characters, so a cut can never leave a prefix of a secret behind.
func cleanError(s string) string {
	return truncRunes(redactSecrets(strings.ToValidUTF8(s, "�")), maxErrorChars)
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (e *Engine) race(point string) {
	if e.raceHook != nil {
		e.raceHook(point)
	}
}

// settleTimeout bounds the write that records a finished provider call.
const settleTimeout = 10 * time.Second

// settleContext is detached from the job context's cancellation (values are kept) with its own deadline.
func settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
}

// yieldToNewState handles a guarded run UPDATE that matched no row: the run changed after it
// was read. A run asked to stop is finished cancelled (completed results kept); any other
// state was written by someone with the right to, and is left alone.
func (e *Engine) yieldToNewState(ctx context.Context, tx pgx.Tx, rs *runState, dest *deststore.Destination) error {
	run, err := deststore.GetRun(ctx, tx, rs.runID)
	if errors.Is(err, deststore.ErrRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if run.Status == "cancelling" || run.Status == "cancelled" {
		_, err = deststore.FinishCancelled(ctx, tx, run, dest)
		return err
	}
	return nil
}
