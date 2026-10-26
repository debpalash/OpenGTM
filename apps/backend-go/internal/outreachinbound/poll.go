package outreachinbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/dsnparse"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/imapclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// maxReconnects bounds reconnects within one tick so a hostile server cannot
// keep a worker busy.
const maxReconnects = 3

// Handler is handle_inbound_poll.
type Handler struct {
	Pool        *pgxpool.Pool
	Secrets     *secrets.Resolver
	Automations bool // AUTOMATIONS_ENABLED
	Outreach    config.OutreachSettings
	Executor    config.ExecutorSettings
	// Dial is the SSRF-guarded dialer (egress.NewDialContext) used by the
	// production client. NewClient overrides client construction (tests).
	Dial      egress.DialContext
	NewClient func(outreachcfg.IMAP) imapclient.Client
	Now       func() time.Time
	Log       *slog.Logger
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) client(cfg outreachcfg.IMAP) (imapclient.Client, error) {
	if h.NewClient != nil {
		return h.NewClient(cfg), nil
	}
	if h.Dial == nil {
		return nil, errors.New("outreachinbound: no egress dialer configured")
	}
	return imapclient.New(cfg, h.Dial, 0), nil
}

func (h *Handler) enabled() bool { return h.Automations && h.Outreach.InboundPollEnabled }

// Handle polls one workspace's mailbox. A failed poll is a failed tick (backoff,
// auto-disable), never a failed job: it returns nil so one bad mailbox cannot
// wedge the worker or other workspaces. Only infrastructure errors (the
// reschedule itself) are returned.
func (h *Handler) Handle(ctx context.Context, c *queue.Claim) error {
	if !h.enabled() {
		return nil
	}
	var p struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.WorkspaceID == "" {
		h.log().Warn("inbound poll missing workspace_id")
		return nil
	}
	ws := p.WorkspaceID
	if c.WorkspaceID != "" && c.WorkspaceID != ws { // as the send handler: the job row's workspace is authoritative
		return &queue.PermanentError{Err: errors.New("outreachinbound: payload workspace does not match the job's workspace")}
	}

	cfg := outreachcfg.LoadIMAP(ctx, h.Secrets, ws)
	if !cfg.Configured() {
		// creds removed or never set: stop scheduling for this workspace
		return outreachstore.DisableInboundSchedule(ctx, h.Pool, ws)
	}

	uidValidity, err := h.poll(ctx, ws, cfg)
	if errors.Is(err, errLeaseBusy) { // another workspace is draining the shared mailbox: nothing to do this tick
		return Reschedule(ctx, h.Pool, ws, false, nil, h.now(), ScheduleConfig{Outreach: h.Outreach, Executor: h.Executor})
	}
	failed := err != nil
	var uv *string
	if failed {
		h.log().Warn("inbound poll failed", "workspace_id", ws, "error", scrub(err, cfg))
	} else {
		uv = &uidValidity
	}
	return Reschedule(ctx, h.Pool, ws, failed, uv, h.now(), ScheduleConfig{Outreach: h.Outreach, Executor: h.Executor})
}

// scrub removes the mailbox password from an error text before it is logged.
func scrub(err error, cfg outreachcfg.IMAP) string {
	return smtpclient.ScrubAuth(err.Error(), cfg.User, cfg.Password.Reveal())
}

// isConnErr is true only for real transport failures; a server NO/BAD reply
// (for example "mailbox closed" text) is not a reason to reconnect.
func isConnErr(err error) bool {
	if errors.Is(err, imapclient.ErrBudgetExceeded) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op)
}

// poll is _run_poll: connect, select, search, then fetch + process each message
// in its own tenant transaction, best-effort MarkSeen after the ledger commit,
// always logout. A message-level error does not stop the batch; the tick is
// reported failed once the batch is done.
func (h *Handler) poll(ctx context.Context, ws string, cfg outreachcfg.IMAP) (string, error) {
	if cfg.Global { // one poller per shared mailbox at a time (session advisory lock, released with the connection)
		conn, err := h.Pool.Acquire(ctx)
		if err != nil {
			return "", err
		}
		defer conn.Release()
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, GlobalLeaseKey(cfg)).Scan(&got); err != nil {
			return "", err
		}
		if !got {
			return "", errLeaseBusy
		}
		defer func() {
			_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, GlobalLeaseKey(cfg))
		}()
	}
	cl, err := h.client(cfg)
	if err != nil {
		return "", err
	}
	defer func() { cl.Logout() }()
	open := func() (string, error) {
		if err := cl.Connect(ctx); err != nil {
			return "", err
		}
		return cl.Select(ctx, cfg.Folder)
	}
	uidValidity, err := open()
	if err != nil {
		return "", err
	}
	days := h.Outreach.InboundLookbackDays
	if days < 1 {
		days = 1
	}
	max := h.Outreach.InboundMaxFetch
	since := h.now().Add(-time.Duration(days) * 24 * time.Hour)
	uids, err := cl.SearchUnseen(ctx, since, max)
	if err != nil {
		return "", err
	}
	if max > 0 && len(uids) > max {
		uids = uids[:max]
	}
	th := outreachstore.ThresholdsFrom(h.Outreach)
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	reconnects := 0
	markSeen := func(uid string) {
		if err := cl.MarkSeen(ctx, uid); err != nil {
			h.log().Warn("inbound mark_seen failed", "workspace_id", ws, "uid", uid, "error", scrub(err, cfg))
		}
	}
	for _, uid := range uids {
		raw, err := cl.Fetch(ctx, uid)
		switch {
		case errors.Is(err, imapclient.ErrOversized):
			// Python's RFC822 fetch marked these seen; never refetch a huge message
			markSeen(uid)
			continue
		case err != nil && isConnErr(err):
			if reconnects >= maxReconnects {
				return "", err
			}
			reconnects++
			hostile := errors.Is(err, imapclient.ErrBudgetExceeded)
			cl.Logout()
			ncl, cerr := h.client(cfg)
			if cerr != nil {
				return "", cerr
			}
			cl = ncl
			nuv, rerr := open()
			if rerr != nil {
				return "", rerr
			}
			uidValidity = nuv
			if hostile {
				markSeen(uid) // the message that blew the budget would do it again
			}
			continue
		case err != nil:
			// Python's fetch() returns b"" for any non-OK reply: skip, never fail the tick
			h.log().Warn("inbound fetch failed, skipping message", "workspace_id", ws, "uid", uid, "error_type", fmt.Sprintf("%T", err))
			continue
		}
		if len(raw) == 0 {
			continue
		}
		target := ws
		if cfg.Global { // a shared mailbox: the send's Message-ID names the owning workspace
			if owner, err := h.owner(ctx, raw); err != nil {
				note(fmt.Errorf("route uid %s: %w", uid, err))
				continue
			} else if owner != "" {
				target = owner
			}
		}
		perr := tenancy.InWorkspace(ctx, h.Pool, target, func(tx pgx.Tx) error {
			return Process(ctx, tx, target, uidValidity, uid, raw, th)
		})
		if perr != nil {
			note(fmt.Errorf("process uid %s: %w", uid, perr))
			continue
		}
		markSeen(uid)
	}
	return uidValidity, firstErr
}

var errLeaseBusy = errors.New("outreachinbound: shared mailbox is being polled by another workspace")

// GlobalLeaseKey names the advisory lock that serialises polls of one shared (global) mailbox.
func GlobalLeaseKey(cfg outreachcfg.IMAP) string {
	return "imap-global:" + cfg.Host + "|" + strconv.Itoa(cfg.Port) + "|" + cfg.User
}

// owner is the workspace owning the first of raw's cited Message-IDs that matches a send (the same
// cross-tenant lookup as the bounce webhook, which returns only the routing tuple); "" when none does,
// and the message is then recorded as unmatched in the polling workspace as before.
func (h *Handler) owner(ctx context.Context, raw []byte) (string, error) {
	for _, r := range dsnparse.Parse(raw) {
		mid := stripNUL(r.OriginalMessageID)
		if mid == "" {
			continue
		}
		ref, err := outreachstore.LookupSendAnyWorkspace(ctx, h.Pool, mid)
		if err != nil {
			return "", err
		}
		if ref != nil {
			return ref.WorkspaceID, nil
		}
	}
	return "", nil
}
