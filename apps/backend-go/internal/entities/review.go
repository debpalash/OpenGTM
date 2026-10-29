package entities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Review decision errors (HTTP 404 / 400 details).
var (
	ErrPairNotFound = errors.New("entities: review pair not found")
	ErrNoCandidate  = errors.New("entities: no candidate entity to merge")
)

// DecideResult is the body of POST /api/entities/review-queue/decide.
type DecideResult struct {
	Status string
	Merge  *MergeResult // set when Status == "merged"
}

// Decide resolves a review pair: "merge" merges the candidate into the pair's entity, anything
// else rejects. The merge and the pair status change commit together (Python commits them in two
// steps).
func (s *Store) Decide(ctx context.Context, ws string, pairID int64, decision string) (DecideResult, error) {
	var res DecideResult
	if pairID < -2147483648 || pairID > 2147483647 {
		return res, ErrPairNotFound
	}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		pair, err := q.LockEntityReviewPair(ctx, gen.LockEntityReviewPairParams{ID: int32(pairID), WorkspaceID: ws})
		if isNoRows(err) {
			return ErrPairNotFound
		}
		if err != nil {
			return err
		}
		if decision != "merge" {
			res = DecideResult{Status: "rejected"}
			return q.SetEntityReviewPairStatus(ctx, gen.SetEntityReviewPairStatusParams{ID: pair.ID, Status: "rejected"})
		}
		cand, _ := asObject(parseOr(pair.Candidate, nil))
		var v any
		if cand != nil {
			v, _ = cand.Get("new_entity_id")
		}
		if !truthy(v) {
			return ErrNoCandidate
		}
		newID, isStr := v.(string)
		if !isStr {
			return opErr(ErrEntityMissing)
		}
		m, err := mergeTx(ctx, q, ws, pair.EntityID.String, newID, "review_confirmed")
		if err != nil {
			return err
		}
		if err := q.SetEntityReviewPairStatus(ctx, gen.SetEntityReviewPairStatusParams{ID: pair.ID, Status: "merged"}); err != nil {
			return err
		}
		res = DecideResult{Status: "merged", Merge: &m}
		return nil
	})
	return res, err
}
