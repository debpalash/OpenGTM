package entities

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// SplitResult is the body of a successful POST /api/entities/split.
type SplitResult struct {
	RestoredID  string `json:"restored_id"`
	ReboundRows int    `json:"rebound_rows"`
}

// Split undoes the merge recorded in log logID (graph.split_entity) in one workspace transaction.
func (s *Store) Split(ctx context.Context, ws string, logID int64) (SplitResult, error) {
	var res SplitResult
	if logID < -2147483648 || logID > 2147483647 {
		return res, opErr(ErrLogMissing)
	}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		var err error
		res, err = splitTx(ctx, gen.New(tx), ws, int32(logID))
		return err
	})
	return res, err
}

// snapText reads an optional text column of the snapshot entity: absent -> def, null -> nil.
func snapText(m *OMap, key, def string) (*string, bool) {
	v, present := m.Get(key)
	if !present {
		return &def, true
	}
	switch x := v.(type) {
	case nil:
		return nil, true
	case string:
		return &x, true
	}
	return nil, false
}

func snapJSON(m *OMap, key string, def []byte) []byte {
	if v, present := m.Get(key); present {
		return MarshalJSON(v)
	}
	return def
}

func snapCount(m *OMap, key string) (*int32, bool) {
	v, present := m.Get(key)
	if !present {
		one := int32(1)
		return &one, true
	}
	if v == nil {
		return nil, true
	}
	n, ok := asInt(v)
	if !ok || n < -2147483648 || n > 2147483647 {
		return nil, false
	}
	c := int32(n)
	return &c, true
}

func splitTx(ctx context.Context, q *gen.Queries, ws string, logID int32) (SplitResult, error) {
	log, err := q.LockEntityMergeLog(ctx, gen.LockEntityMergeLogParams{ID: logID, WorkspaceID: ws})
	if isNoRows(err) || (err == nil && log.Reverted.Valid && log.Reverted.Int32 != 0) {
		return SplitResult{}, opErr(ErrLogMissing)
	}
	if err != nil {
		return SplitResult{}, err
	}
	snap := objOr(log.Snapshot)
	entData := objOrEmpty(getOr(snap, "entity", nil))
	snapWS, _ := getOr(entData, "workspace_id", nil).(string)
	if snapWS != ws {
		return SplitResult{}, opErr(ErrLogMissing)
	}
	eid, _ := getOr(entData, "id", nil).(string)
	if eid == "" {
		return SplitResult{}, opErr(ErrBadSnapshot)
	}
	// Lock the surviving entity first, like mergeTx does, so split and merge into the same kept
	// entity never take entity, identifier and row locks in opposite orders.
	keptID := ""
	if log.KeptID.Valid {
		keptID = log.KeptID.String
	}
	var keptRow *gen.CompanyEntity
	if row, err := q.LockCompanyEntity(ctx, gen.LockCompanyEntityParams{ID: keptID, WorkspaceID: ws}); err == nil {
		keptRow = &row
	} else if !isNoRows(err) {
		return SplitResult{}, err
	}
	name, _ := getOr(entData, "canonical_name", nil).(string)
	if name == "" {
		name = "(unknown)"
	}
	p := gen.InsertCompanyEntityParams{ID: eid, WorkspaceID: snapWS, CanonicalName: name}
	var ok [4]bool
	var d, ph, em, ci *string
	d, ok[0] = snapText(entData, "primary_domain", "")
	ph, ok[1] = snapText(entData, "primary_phone", "")
	em, ok[2] = snapText(entData, "primary_email", "")
	ci, ok[3] = snapText(entData, "primary_city", "")
	for _, good := range ok {
		if !good {
			return SplitResult{}, opErr(ErrBadSnapshot)
		}
	}
	p.PrimaryDomain, p.PrimaryPhone, p.PrimaryEmail, p.PrimaryCity = pgText(d), pgText(ph), pgText(em), pgText(ci)
	p.IdentityKeys = snapJSON(entData, "identity_keys", []byte("{}"))
	p.Fields = snapJSON(entData, "fields", []byte("{}"))
	p.Sources = snapJSON(entData, "sources", []byte("[]"))
	p.SourceAgreement = snapJSON(entData, "source_agreement", []byte("{}"))
	cc, good1 := snapCount(entData, "corroboration_count")
	oc, good2 := snapCount(entData, "observation_count")
	if !good1 || !good2 {
		return SplitResult{}, opErr(ErrBadSnapshot)
	}
	p.CorroborationCount, p.ObservationCount = pgI32(cc), pgI32(oc)
	if err := q.InsertCompanyEntity(ctx, p); err != nil {
		return SplitResult{}, err
	}

	var keys []string
	for _, k := range listOf(getOr(snap, "blocking_keys", nil)) {
		if s, ok := k.(string); ok {
			keys = append(keys, s)
		}
	}
	sort.Strings(keys)
	if err := ensureBlockingKeys(ctx, q, eid, keys, snapWS); err != nil {
		return SplitResult{}, err
	}
	for _, pair := range listOf(getOr(snap, "identifiers", nil)) {
		kv := listOf(pair)
		if len(kv) != 2 {
			continue
		}
		kind, ok1 := kv[0].(string)
		value, ok2 := kv[1].(string)
		if !ok1 || !ok2 {
			continue
		}
		if err := q.RestoreCompanyIdentifier(ctx, gen.RestoreCompanyIdentifierParams{EntityID: eid, WorkspaceID: snapWS, Kind: kind, Value: value}); err != nil {
			return SplitResult{}, err
		}
	}
	rowIDs := listOf(getOr(snap, "row_ids", nil))
	for _, r := range rowIDs {
		if n, ok := asInt(r); ok && n >= -2147483648 && n <= 2147483647 {
			if err := q.RebindWorkbookRow(ctx, gen.RebindWorkbookRowParams{EntityID: eid, ID: int32(n), WorkspaceID: snapWS}); err != nil {
				return SplitResult{}, err
			}
		}
	}
	if refs, ok := asObject(getOr(snap, "references", nil)); ok && refs.Len() > 0 {
		if err := restoreAccounts(ctx, q, snapWS, refs, eid, keptID); err != nil {
			return SplitResult{}, err
		}
	}
	if keptRow != nil {
		kept := fromRow(*keptRow)
		restoreEvidence(kept, snap)
		if err := kept.save(ctx, q); err != nil {
			return SplitResult{}, err
		}
	}
	if prefs, ok := asObject(getOr(snap, "person_references", nil)); ok && prefs.Len() > 0 {
		if err := restorePeople(ctx, q, snapWS, prefs, eid, keptID); err != nil {
			return SplitResult{}, err
		}
	}
	if shared, ok := asObject(getOr(snap, "shared_records", nil)); ok && shared.Len() > 0 {
		if e := q.RestoreSharedCompanyRecords(ctx, gen.RestoreSharedCompanyRecordsParams{WorkspaceID: ws, Undo: MarshalJSON(shared)}); e != nil {
			return SplitResult{}, e
		}
	}
	if err := q.MarkEntityMergeLogReverted(ctx, gen.MarkEntityMergeLogRevertedParams{ID: logID, WorkspaceID: ws}); err != nil {
		return SplitResult{}, err
	}
	return SplitResult{RestoredID: eid, ReboundRows: len(rowIDs)}, nil
}
