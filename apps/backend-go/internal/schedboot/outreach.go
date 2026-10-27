package schedboot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Outreach is bootstrap_outreach_schedules (AUTOMATIONS_ENABLED): tick every due sequence (enqueue its due
// sends and reschedule). It returns the send jobs enqueued.
func (d Deps) Outreach(ctx context.Context) (int, error) {
	if !d.Cfg.Flags.AutomationsEnabled {
		return 0, nil
	}
	rs, err := d.Pool.Query(ctx, `SELECT sequence_id, workspace_id FROM outreach_schedules
	    WHERE enabled AND (next_tick_at IS NULL OR next_tick_at <= $1) ORDER BY sequence_id`, d.now())
	if err != nil {
		return 0, fmt.Errorf("schedboot: read outreach schedules: %w", err)
	}
	type due struct{ seq, ws string }
	var all []due
	for rs.Next() {
		var x due
		if err := rs.Scan(&x.seq, &x.ws); err != nil {
			rs.Close()
			return 0, err
		}
		all = append(all, x)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return 0, err
	}
	o := d.Cfg.Flags.Outreach
	cfg := outreachsend.TickConfig{Interval: time.Duration(o.TickInterval) * time.Second, MaxEnqueue: o.TickMaxEnqueue,
		Executors: d.Cfg.Executor, Outreach: o}
	var rows perRow
	n := 0
	for _, x := range all {
		sent, err := outreachsend.Tick(ctx, d.Pool, x.ws, x.seq, d.now(), cfg)
		if err != nil {
			rows.add("sequence", x.seq, err) // one bad sequence never breaks the bootstrap
			continue
		}
		n += sent
	}
	d.log().Info("bootstrap_outreach_schedules", "enqueued", n)
	if err := rows.err(); err != nil {
		return n, fmt.Errorf("schedboot: outreach: %w", err)
	}
	return n, nil
}

// OutreachInbound is bootstrap_inbound_schedules (AUTOMATIONS_ENABLED and OUTREACH_INBOUND_POLL_ENABLED):
// poll every workspace whose inbound mirror is due, and seed one for each IMAP-configured workspace that
// has an enabled outreach schedule but no inbound row yet. A due workspace without IMAP credentials is
// disabled. It returns the polls enqueued.
func (d Deps) OutreachInbound(ctx context.Context) (int, error) {
	o := d.Cfg.Flags.Outreach
	if !d.Cfg.Flags.AutomationsEnabled || !o.InboundPollEnabled {
		return 0, nil
	}
	if d.IMAPConfigured == nil {
		return 0, errors.New("schedboot: inbound bootstrap needs the workspace secret store (CREDENTIALS_SOURCE=local)")
	}
	now := d.now()
	dueWS, err := d.stringCol(ctx, `SELECT workspace_id FROM outreach_inbound_schedules
	    WHERE enabled AND (next_poll_at IS NULL OR next_poll_at <= $1) ORDER BY workspace_id`, now)
	if err != nil {
		return 0, err
	}
	seedWS, err := d.stringCol(ctx, `SELECT DISTINCT workspace_id FROM outreach_schedules s
	    WHERE enabled AND NOT EXISTS (SELECT 1 FROM outreach_inbound_schedules i WHERE i.workspace_id = s.workspace_id)
	    ORDER BY workspace_id`)
	if err != nil {
		return 0, err
	}
	var rows perRow
	seen := map[string]bool{}
	var order []string
	add := func(ws string) {
		if !seen[ws] {
			seen[ws] = true
			order = append(order, ws)
		}
	}
	for _, ws := range dueWS {
		add(ws)
	}
	for _, ws := range seedWS {
		ok, err := d.IMAPConfigured(ctx, ws)
		if err != nil {
			rows.add("workspace", ws, err)
			continue
		}
		if ok {
			add(ws)
		}
	}
	cfg := outreachinbound.ScheduleConfig{Outreach: o, Executor: d.Cfg.Executor}
	n := 0
	for _, ws := range order {
		ok, err := d.IMAPConfigured(ctx, ws)
		if err == nil && !ok {
			err = disableInbound(ctx, d, ws)
		} else if err == nil {
			err = outreachinbound.Reschedule(ctx, d.Pool, ws, false, nil, now, cfg)
			if err == nil {
				n++
			}
		}
		if err != nil {
			rows.add("workspace", ws, err)
		}
	}
	d.log().Info("bootstrap_inbound_schedules", "enqueued", n)
	if err := rows.err(); err != nil {
		return n, fmt.Errorf("schedboot: outreach_inbound: %w", err)
	}
	return n, nil
}

// disableInbound is _disable: the mirror row is created if missing, then switched off with no next poll
// (the failure counter is kept).
func disableInbound(ctx context.Context, d Deps, ws string) error {
	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		return outreachstore.UpsertInboundSchedule(ctx, tx, ws, outreachstore.InboundScheduleUpsert{Enabled: false})
	})
}

func (d Deps) stringCol(ctx context.Context, sql string, args ...any) ([]string, error) {
	rs, err := d.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("schedboot: %w", err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rs.Err()
}
