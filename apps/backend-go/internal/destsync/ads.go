package destsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/adsdrv"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
)

// adErrorChars is Python's str(exc)[:500] for a failed platform call.
const adErrorChars = 500

// flushAds is _sync_ad_candidates: one bounded batch (add or remove). Order: lease, cancel
// check (an ended run stops here and its pending rows become cancelled), rows to in_flight with
// attempts+1, ONE platform request with no transaction open, then every row settled to the
// shared outcome. Returns stop=true when the run ended cancelled.
func (e *Engine) flushAds(ctx context.Context, rs *runState, op string, cands []adCand) (stop bool, err error) {
	if len(cands) == 0 {
		return false, nil
	}
	if err := e.guard(ctx, rs); err != nil {
		return true, err
	}
	ids := make([]int32, len(cands))
	rows := make([]map[string]string, len(cands))
	for i, c := range cands {
		ids[i], rows[i] = c.id, c.row
	}
	var cancelled bool
	e.tick(rs)
	err = e.inTx(ctx, rs, func(tx pgx.Tx) error {
		c, err := e.checkInTx(ctx, tx, rs)
		if err != nil || c {
			cancelled = c
			return err
		}
		return gen.New(tx).DSDeliveriesMarkInFlight(ctx, gen.DSDeliveriesMarkInFlightParams{WorkspaceID: rs.ws, Ids: ids})
	})
	if err != nil || cancelled {
		return true, err
	}
	rs.st.attempted += len(cands)

	res := e.callAds(ctx, rs, op, rows)

	// Detached from the job context: a finished platform call is always recorded.
	sctx, cancel := settleContext(ctx)
	defer cancel()
	err = e.inTx(sctx, rs, func(tx pgx.Tx) error {
		_, err := gen.New(tx).DSDeliveriesSettle(sctx, settleParams(rs.ws, ids, res.Success, res.Summary, res.Error, res.ExternalID))
		return err
	})
	if err != nil {
		return true, fmt.Errorf("destsync: settle ad batch: %w", err)
	}
	if res.Success {
		rs.st.succeeded += len(cands)
	} else {
		rs.st.failed += len(cands)
	}
	return false, ctx.Err()
}

func settleParams(ws string, ids []int32, ok bool, summary, errText, external string) gen.DSDeliveriesSettleParams {
	p := gen.DSDeliveriesSettleParams{WorkspaceID: ws, Ids: ids, Status: "failed", Summary: pgtype.Text{String: summary, Valid: true}, Delivered: ok}
	if ok {
		p.Status = "success"
	}
	if t := cleanError(errText); t != "" {
		p.Error = pgtype.Text{String: t, Valid: true}
	}
	if external != "" {
		p.ExternalID = pgtype.Text{String: external, Valid: true}
	}
	return p
}

// callAds makes the platform request. A returned error (Python raised) is the failed result
// with its text redacted and cut to 500 characters; a panic gets a fixed text.
func (e *Engine) callAds(ctx context.Context, rs *runState, op string, rows []map[string]string) (res adsdrv.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = adsdrv.Result{Error: "ad batch failed unexpectedly"}
		}
	}()
	r, err := e.d.Ads.SyncRows(ctx, rs.ws, rs.dest.Type, rs.cfg, rows, op)
	if err != nil {
		return adsdrv.Result{Error: truncRunes(cleanError(err.Error()), adErrorChars)}
	}
	return r
}

// reconcileExits removes people who left the audience. For each lead whose latest successful
// delivery is an upsert and who has no membership row now, a remove is sent over the stored
// hashed payload (never a raw identifier). A remove that already succeeded is skipped; one
// that failed earlier is queued again where it is. Candidates are read 500 at a time (each
// page its own transaction) and sent in batches of AdBatchSize.
func (e *Engine) reconcileExits(ctx context.Context, rs *runState) (stop bool, err error) {
	var batch []adCand
	var after int32
	for {
		var page []gen.DSExitCandidatesRow
		err := e.inTx(ctx, rs, func(tx pgx.Tx) error {
			var err error
			page, err = gen.New(tx).DSExitCandidates(ctx, gen.DSExitCandidatesParams{
				WorkspaceID: rs.ws, DestinationID: rs.dest.ID, AudienceID: rs.dest.AudienceID,
				AfterID: after, PageLimit: int32(e.d.MemberPageSize),
			})
			return err
		})
		if err != nil {
			return true, fmt.Errorf("destsync: exit candidates: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.ID
			prior, err := decodeObj(row.Payload)
			if err != nil {
				return true, fmt.Errorf("destsync: stored ad payload: %w", err)
			}
			if len(prior) == 0 {
				continue
			}
			fp := destdeliver.Fingerprint(prior)
			idem := destdeliver.IdempotencyKey(rs.dest.ID, int(row.LeadID), fp, true)
			id, skip, err := e.queueRemoval(ctx, rs, int(row.LeadID), idem, fp, row.Payload)
			if err != nil {
				return true, err
			}
			if skip {
				rs.st.skipped++
				continue
			}
			batch = append(batch, adCand{id: id, row: adsdrv.IdentifierRow("remove", prior)})
			if len(batch) >= e.d.AdBatchSize {
				if stop, err := e.flushAds(ctx, rs, "remove", batch); err != nil || stop {
					return true, err
				}
				batch = batch[:0]
			}
		}
	}
	if len(batch) > 0 {
		return e.flushAds(ctx, rs, "remove", batch)
	}
	return false, nil
}

// queueRemoval creates or revives the remove row (pending). skip=true: that removal already
// succeeded.
func (e *Engine) queueRemoval(ctx context.Context, rs *runState, lead int, idem, fp string, payload []byte) (id int32, skip bool, err error) {
	err = e.inTx(ctx, rs, func(tx pgx.Tx) error {
		q := gen.New(tx)
		prior, err := q.DestDeliveryByKey(ctx, gen.DestDeliveryByKeyParams{WorkspaceID: rs.ws, IdempotencyKey: idem})
		switch {
		case err == nil:
			if prior.Status == "success" {
				skip = true
				return nil
			}
			id = prior.ID
			return q.DSDeliveryRequeue(ctx, gen.DSDeliveryRequeueParams{WorkspaceID: rs.ws, ID: prior.ID})
		case errors.Is(err, pgx.ErrNoRows):
			d, _, err := deststore.UpsertDelivery(ctx, tx, deststore.UpsertDeliveryInput{
				RunID: rs.runID, DestinationID: rs.dest.ID, LeadID: lead, Operation: "remove", IdempotencyKey: idem,
				Fingerprint: fp, Payload: json.RawMessage(payload), Status: "pending",
			})
			id = int32(d.ID)
			return err
		default:
			return err
		}
	})
	return id, skip, err
}
