package entities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Account and person reference rewriting for merge (graph._repoint_*) and split (graph._restore_*).

// scopeKey is signals.tracking._scope_key.
func scopeKey(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "|")))
	return "accounts_" + hex.EncodeToString(sum[:])[:24]
}

func isAccount(a any, id string) bool {
	m, ok := asObject(a)
	if !ok {
		return false
	}
	v, _ := m.Get("account_id")
	s, isStr := v.(string)
	return isStr && s == id
}

// repointAccounts rewrites every stored account reference at kept and returns the undo record.
func repointAccounts(ctx context.Context, q *gen.Queries, ws, merged, kept string) (*OMap, error) {
	undo := NewOMap()
	accountRows := []any{}
	rows, err := q.ListAccountReferenceRows(ctx, gen.ListAccountReferenceRowsParams{WorkspaceID: ws, EntityID: merged})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		data, ok := asObject(parseOr(r.Data, nil))
		if !ok || data.Len() == 0 || !isAccountField(data, merged) {
			continue
		}
		nd := data.Clone()
		nd.Set("account_id", kept)
		if err := q.UpdateWorkbookRowData(ctx, gen.UpdateWorkbookRowDataParams{ID: r.ID, WorkspaceID: ws, Data: MarshalJSON(nd)}); err != nil {
			return nil, err
		}
		accountRows = append(accountRows, r.ID)
	}
	undo.Set("account_rows", accountRows)
	watches := NewOMap()
	undo.Set("watches", watches)

	ws2, err := q.ListAccountGroupWatches(ctx, ws)
	if err != nil {
		return nil, err
	}
	for _, w := range ws2 {
		config := objOr(w.Config)
		accounts := listOf(getOr(config, "accounts", nil))
		has := false
		for _, a := range accounts {
			if isAccount(a, merged) {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		cursor := objOr(w.Cursor)
		group := shallowObj(getOr(cursor, "account_group", nil))
		health := shallowObj(getOr(cursor, "collector_health", nil))
		keptTracked := false
		for _, a := range accounts {
			if isAccount(a, kept) {
				keptTracked = true
				break
			}
		}
		saved := NewOMap()
		saved.Set("accounts", deepCopy(accounts))
		saved.Set("scope_key", getOr(config, "scope_key", nil))
		ge, hasGroup := group.Get(merged)
		saved.Set("group_entry", deepCopy(ge))
		savedHealth := NewOMap()
		prefix := merged + ":"
		for _, k := range health.keys {
			if strings.HasPrefix(k, prefix) {
				savedHealth.Set(k, deepCopy(health.vals[k]))
			}
		}
		saved.Set("health", savedHealth)
		saved.Set("moved_cursor", !keptTracked && hasGroup)
		watches.Set(w.ID, saved)

		newAccounts := []any{}
		for _, a := range accounts {
			if isAccount(a, merged) {
				if keptTracked {
					continue
				}
				c := a.(*OMap).Clone()
				c.Set("account_id", kept)
				a = c
			}
			newAccounts = append(newAccounts, a)
		}
		if hasGroup {
			group.Delete(merged)
			group.SetDefault(kept, ge)
		}
		for _, k := range savedHealth.keys {
			v := health.vals[k]
			health.Delete(k)
			health.SetDefault(kept+":"+strings.SplitN(k, ":", 2)[1], v)
		}
		var ids []string
		for _, a := range newAccounts {
			if m, ok := asObject(a); ok {
				if s, isStr := getOr(m, "account_id", nil).(string); isStr && s != "" {
					ids = append(ids, s)
				}
			}
		}
		nc := config.Clone()
		nc.Set("accounts", newAccounts)
		nc.Set("scope_key", scopeKey(ids))
		ncur := cursor.Clone()
		ncur.Set("account_group", group)
		ncur.Set("collector_health", health)
		if err := q.UpdateWatchConfigCursor(ctx, gen.UpdateWatchConfigCursorParams{
			ID: w.ID, WorkspaceID: ws, Config: MarshalJSON(nc), Cursor: MarshalJSON(ncur)}); err != nil {
			return nil, err
		}
	}
	return undo, nil
}

func isAccountField(data *OMap, id string) bool {
	v, _ := data.Get("account_id")
	s, ok := v.(string)
	return ok && s == id
}

func shallowObj(v any) *OMap {
	if m, ok := asObject(v); ok && m.Len() > 0 {
		return m.Clone()
	}
	return NewOMap()
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case *OMap:
		c := NewOMap()
		for _, k := range x.keys {
			c.Set(k, deepCopy(x.vals[k]))
		}
		return c
	case []any:
		c := make([]any, len(x))
		for i, e := range x {
			c[i] = deepCopy(e)
		}
		return c
	}
	return v
}

// restoreAccounts reverses repointAccounts for a split.
func restoreAccounts(ctx context.Context, q *gen.Queries, ws string, undo *OMap, merged, kept string) error {
	for _, id := range listOf(getOr(undo, "account_rows", nil)) {
		n, ok := asInt(id)
		if !ok {
			continue
		}
		raw, err := q.GetWorkbookRowData(ctx, gen.GetWorkbookRowDataParams{ID: int32(n), WorkspaceID: ws})
		if err != nil {
			if isNoRows(err) {
				continue
			}
			return err
		}
		data, ok := asObject(parseOr(raw, nil))
		if !ok || data.Len() == 0 || !isAccountField(data, kept) {
			continue
		}
		nd := data.Clone()
		nd.Set("account_id", merged)
		if err := q.UpdateWorkbookRowData(ctx, gen.UpdateWorkbookRowDataParams{ID: int32(n), WorkspaceID: ws, Data: MarshalJSON(nd)}); err != nil {
			return err
		}
	}
	watches := objOrEmpty(getOr(undo, "watches", nil))
	for _, wid := range watches.keys {
		saved, ok := asObject(watches.vals[wid])
		if !ok {
			continue
		}
		w, err := q.GetWatchForRestore(ctx, gen.GetWatchForRestoreParams{ID: wid, WorkspaceID: ws})
		if err != nil {
			if isNoRows(err) {
				continue
			}
			return err
		}
		config := objOr(w.Config)
		cursor := objOr(w.Cursor)
		group := shallowObj(getOr(cursor, "account_group", nil))
		health := shallowObj(getOr(cursor, "collector_health", nil))
		if truthy(getOr(saved, "moved_cursor", nil)) {
			group.Delete(kept)
			for _, k := range health.Keys() {
				if strings.HasPrefix(k, kept+":") {
					health.Delete(k)
				}
			}
		}
		if ge := getOr(saved, "group_entry", nil); ge != nil {
			group.Set(merged, ge)
		}
		if sh, ok := asObject(getOr(saved, "health", nil)); ok {
			for _, k := range sh.keys {
				health.Set(k, sh.vals[k])
			}
		}
		accounts := getOr(saved, "accounts", nil)
		if !truthy(accounts) {
			accounts = []any{}
		}
		nc := config.Clone()
		nc.Set("accounts", accounts)
		nc.Set("scope_key", getOr(saved, "scope_key", nil))
		ncur := cursor.Clone()
		ncur.Set("account_group", group)
		ncur.Set("collector_health", health)
		if err := q.UpdateWatchConfigCursor(ctx, gen.UpdateWatchConfigCursorParams{
			ID: wid, WorkspaceID: ws, Config: MarshalJSON(nc), Cursor: MarshalJSON(ncur)}); err != nil {
			return err
		}
	}
	return nil
}

// repointPeople moves current and historical person associations to kept; returns the undo ids.
func repointPeople(ctx context.Context, q *gen.Queries, ws, merged, kept string) (*OMap, error) {
	people, err := q.ListCompanyPersonIDs(ctx, gen.ListCompanyPersonIDsParams{WorkspaceID: ws, CompanyID: merged})
	if err != nil {
		return nil, err
	}
	emps, err := q.ListCompanyEmploymentIDs(ctx, gen.ListCompanyEmploymentIDsParams{WorkspaceID: ws, CompanyID: merged})
	if err != nil {
		return nil, err
	}
	if err := q.RepointPersonEntities(ctx, gen.RepointPersonEntitiesParams{WorkspaceID: ws, MergedID: merged, KeptID: kept}); err != nil {
		return nil, err
	}
	if err := q.RepointPersonEmployments(ctx, gen.RepointPersonEmploymentsParams{WorkspaceID: ws, MergedID: merged, KeptID: kept}); err != nil {
		return nil, err
	}
	undo := NewOMap()
	pa := make([]any, len(people))
	for i, p := range people {
		pa[i] = p
	}
	ea := make([]any, len(emps))
	for i, e := range emps {
		ea[i] = e
	}
	undo.Set("people", pa)
	undo.Set("employments", ea)
	return undo, nil
}

// restorePeople undoes only moved associations that still point at this merge's survivor.
func restorePeople(ctx context.Context, q *gen.Queries, ws string, undo *OMap, merged, kept string) error {
	var people []string
	for _, v := range listOf(getOr(undo, "people", nil)) {
		if s, ok := v.(string); ok {
			people = append(people, s)
		}
	}
	var emps []int32
	for _, v := range listOf(getOr(undo, "employments", nil)) {
		if n, ok := asInt(v); ok && n >= -2147483648 && n <= 2147483647 {
			emps = append(emps, int32(n))
		}
	}
	if len(people) > 0 {
		if err := q.RestorePersonEntities(ctx, gen.RestorePersonEntitiesParams{WorkspaceID: ws, MergedID: merged, KeptID: kept, Ids: people}); err != nil {
			return err
		}
	}
	if len(emps) > 0 {
		if err := q.RestorePersonEmployments(ctx, gen.RestorePersonEmploymentsParams{WorkspaceID: ws, MergedID: merged, KeptID: kept, Ids: emps}); err != nil {
			return err
		}
	}
	return nil
}
