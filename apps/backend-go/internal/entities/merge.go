package entities

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// MergeResult is the body of a successful POST /api/entities/merge.
type MergeResult struct {
	KeptID             string `json:"kept_id"`
	MergedID           string `json:"merged_id"`
	CorroborationCount *int32 `json:"corroboration_count"`
}

// Merge folds mergedID into keptID inside one workspace transaction (graph.merge_entities).
// Failures that Python answers as {"error": ...} come back as *OpError and write nothing.
func (s *Store) Merge(ctx context.Context, ws, keptID, mergedID, reason string) (MergeResult, error) {
	var res MergeResult
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		var err error
		res, err = mergeTx(ctx, gen.New(tx), ws, keptID, mergedID, reason)
		return err
	})
	return res, err
}

// lockPair takes both entity row locks in stable id order and returns the freshly read rows.
// A row of another workspace is invisible (RLS plus the explicit workspace filter) and reads as missing.
func lockPair(ctx context.Context, q *gen.Queries, ws, a, b string) (*state, *state, error) {
	ids := []string{a, b}
	sort.Strings(ids)
	got := map[string]*state{}
	for _, id := range ids {
		row, err := q.LockCompanyEntity(ctx, gen.LockCompanyEntityParams{ID: id, WorkspaceID: ws})
		if isNoRows(err) {
			return nil, nil, opErr(ErrEntityMissing)
		}
		if err != nil {
			return nil, nil, err
		}
		got[id] = fromRow(row)
	}
	return got[a], got[b], nil
}

func mergeTx(ctx context.Context, q *gen.Queries, ws, keptID, mergedID, reason string) (MergeResult, error) {
	if keptID == mergedID {
		return MergeResult{}, opErr(ErrSameEntity)
	}
	// Deviation from Python: an id owned by another workspace answers entity_not_found like a missing
	// id (Python answers cross_workspace_merge_forbidden, which confirms the foreign id exists).
	kept, merged, err := lockPair(ctx, q, ws, keptID, mergedID)
	if err != nil {
		return MergeResult{}, err
	}
	if !fieldsAreLists(kept.Fields) || !fieldsAreLists(merged.Fields) {
		// Python raises TypeError/AttributeError (HTTP 500) on provenance that is not field -> list.
		return MergeResult{}, opErr(ErrBadFields)
	}

	// Snapshot for split().
	keys, err := q.ListEntityBlockingKeys(ctx, gen.ListEntityBlockingKeysParams{EntityID: mergedID, WorkspaceID: ws})
	if err != nil {
		return MergeResult{}, err
	}
	rowIDs, err := q.ListEntityRowIDs(ctx, gen.ListEntityRowIDsParams{EntityID: mergedID, WorkspaceID: ws})
	if err != nil {
		return MergeResult{}, err
	}
	idents, err := q.ListEntityIdentifiers(ctx, gen.ListEntityIdentifiersParams{EntityID: mergedID, WorkspaceID: ws})
	if err != nil {
		return MergeResult{}, err
	}
	snap := NewOMap()
	snap.Set("entity", merged.api())
	snap.Set("kept_entity", kept.api())
	snap.Set("blocking_keys", strs(keys))
	snap.Set("row_ids", ints(rowIDs))
	pairs := make([]any, len(idents))
	for i, id := range idents {
		pairs[i] = []any{id.Kind, id.Value}
	}
	snap.Set("identifiers", pairs)
	refs, err := repointAccounts(ctx, q, ws, mergedID, keptID)
	if err != nil {
		return MergeResult{}, err
	}
	snap.Set("references", refs)
	prefs, err := repointPeople(ctx, q, ws, mergedID, keptID)
	if err != nil {
		return MergeResult{}, err
	}
	snap.Set("person_references", prefs)

	mergeEvidence(kept, merged)
	if err := q.RepointCompanyIdentifiers(ctx, gen.RepointCompanyIdentifiersParams{KeptID: keptID, MergedID: mergedID, WorkspaceID: ws}); err != nil {
		return MergeResult{}, err
	}
	if err := ensureBlockingKeys(ctx, q, keptID, keys, ws); err != nil {
		return MergeResult{}, err
	}
	if err := q.RepointWorkbookRows(ctx, gen.RepointWorkbookRowsParams{KeptID: keptID, MergedID: mergedID, WorkspaceID: ws}); err != nil {
		return MergeResult{}, err
	}
	score := CompareLeads(merged.lead(), kept.lead())
	if _, err := q.InsertEntityMergeLog(ctx, gen.InsertEntityMergeLogParams{
		WorkspaceID: ws, KeptID: keptID, MergedID: mergedID, Reason: reason, Score: score, Snapshot: MarshalJSON(snap)}); err != nil {
		return MergeResult{}, err
	}
	if err := q.DeleteEntityBlockingKeys(ctx, gen.DeleteEntityBlockingKeysParams{EntityID: mergedID, WorkspaceID: ws}); err != nil {
		return MergeResult{}, err
	}
	if err := q.DeleteCompanyEntity(ctx, gen.DeleteCompanyEntityParams{ID: mergedID, WorkspaceID: ws}); err != nil {
		return MergeResult{}, err
	}
	kept.recompute()
	if err := kept.save(ctx, q); err != nil {
		return MergeResult{}, err
	}
	return MergeResult{KeptID: keptID, MergedID: mergedID, CorroborationCount: kept.Corroboration}, nil
}

// ensureBlockingKeys inserts the keys the entity does not own yet (graph._ensure_blocking_keys).
func ensureBlockingKeys(ctx context.Context, q *gen.Queries, entityID string, keys []string, ws string) error {
	if len(keys) == 0 {
		return nil
	}
	existing, err := q.ListEntityBlockingKeys(ctx, gen.ListEntityBlockingKeysParams{EntityID: entityID, WorkspaceID: ws})
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, k := range existing {
		have[k] = true
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var add []string
	for k := range want {
		if !have[k] {
			add = append(add, k)
		}
	}
	sort.Strings(add)
	for _, k := range add {
		if err := q.InsertEntityBlockingKey(ctx, gen.InsertEntityBlockingKeyParams{Key: k, EntityID: entityID, WorkspaceID: ws}); err != nil {
			return err
		}
	}
	return nil
}

func strs(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func ints(in []int32) []any {
	out := make([]any, len(in))
	for i, n := range in {
		out[i] = n
	}
	return out
}
