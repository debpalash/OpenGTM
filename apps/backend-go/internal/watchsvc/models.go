// Package watchsvc is the intent-poller watch service: validation, cursor,
// CRUD, poll-now and the signal feed (routers/watches.py). Every tenant
// statement runs inside tenancy.InWorkspace.
package watchsvc

import (
	"encoding/json"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Watch is one watch_subscriptions row (the fields _to_api reads).
type Watch struct {
	CompanyID, PersonID           *string
	ID, WorkspaceID, Kind, Target string
	ResolvedCIK                   *string
	LeadID                        *int32
	SignalTypes                   any // ordered JSON value
	Interval                      *string
	Config                        any
	Enabled                       bool
	NextPollAt, LastPolledAt      *time.Time
	LastError                     *string
	ConsecutiveFailures           int32
	Cursor                        any
	CreatedAt                     *time.Time
}

func toWatch(r gen.WatchSubscription) (Watch, error) {
	w := Watch{ID: r.ID, WorkspaceID: r.WorkspaceID, Kind: r.Kind, Target: r.Target, Enabled: r.Enabled,
		ConsecutiveFailures: r.ConsecutiveFailures.Int32}
	if r.CompanyEntityID.Valid {
		v := r.CompanyEntityID.String
		w.CompanyID = &v
	}
	if r.PersonEntityID.Valid {
		v := r.PersonEntityID.String
		w.PersonID = &v
	}
	if r.ResolvedCik.Valid {
		s := r.ResolvedCik.String
		w.ResolvedCIK = &s
	}
	if r.LeadID.Valid {
		n := r.LeadID.Int32
		w.LeadID = &n
	}
	if r.Interval.Valid {
		s := r.Interval.String
		w.Interval = &s
	}
	if r.LastError.Valid {
		s := r.LastError.String
		w.LastError = &s
	}
	for _, p := range []struct {
		dst **time.Time
		src gen.WatchSubscription
		v   func(gen.WatchSubscription) (time.Time, bool)
	}{
		{&w.NextPollAt, r, func(x gen.WatchSubscription) (time.Time, bool) { return x.NextPollAt.Time, x.NextPollAt.Valid }},
		{&w.LastPolledAt, r, func(x gen.WatchSubscription) (time.Time, bool) { return x.LastPolledAt.Time, x.LastPolledAt.Valid }},
		{&w.CreatedAt, r, func(x gen.WatchSubscription) (time.Time, bool) { return x.CreatedAt.Time, x.CreatedAt.Valid }},
	} {
		if t, ok := p.v(p.src); ok {
			u := t.UTC()
			*p.dst = &u
		}
	}
	var err error
	if w.SignalTypes, err = autosvc.ParseJSON(r.SignalTypes); err != nil {
		return w, err
	}
	if w.Config, err = autosvc.ParseJSON(r.Config); err != nil {
		return w, err
	}
	if w.Cursor, err = autosvc.ParseJSON(r.Cursor); err != nil {
		return w, err
	}
	return w, nil
}

func isoOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return autosvc.Iso(*t)
}

func orEmptyObj(v any) any {
	if o, ok := v.(*wbcrud.Obj); ok && o.Len() > 0 {
		return o
	}
	return wbcrud.NewObj()
}

func orEmptyList(v any) any {
	if a, ok := v.([]any); ok && len(a) > 0 {
		return a
	}
	return []any{}
}

func objGet(v any, k string) any {
	o, ok := v.(*wbcrud.Obj)
	if !ok {
		return nil
	}
	x, _ := o.Get(k)
	return x
}

func truthyStr(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case json.Number:
		return t != "0" && t != "0.0"
	case []any:
		return len(t) > 0
	case *wbcrud.Obj:
		return t.Len() > 0
	}
	return true
}

// pyIntOrZero is int(x or 0) for decoded JSON; values Python's int() rejects
// count as 0 (Python would 500).
func pyIntOrZero(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
	case bool:
		if t {
			return 1
		}
	case string:
		if n, ok := pyIntStr(t); ok {
			return n
		}
	}
	return 0
}

// Obj renders _to_api in key order (account_group extras included).
func (w Watch) Obj() *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set("id", w.ID)
	o.Set("workspace_id", w.WorkspaceID)
	if w.CompanyID != nil {
		o.Set("company_id", *w.CompanyID)
	}
	if w.PersonID != nil {
		o.Set("person_id", *w.PersonID)
	}
	o.Set("kind", w.Kind)
	o.Set("target", w.Target)
	if w.ResolvedCIK == nil {
		o.Set("resolved_cik", nil)
	} else {
		o.Set("resolved_cik", *w.ResolvedCIK)
	}
	if w.LeadID == nil {
		o.Set("lead_id", nil)
	} else {
		o.Set("lead_id", int64(*w.LeadID))
	}
	o.Set("signal_types", orEmptyList(w.SignalTypes))
	if w.Interval == nil {
		o.Set("interval", nil)
	} else {
		o.Set("interval", *w.Interval)
	}
	o.Set("config", orEmptyObj(w.Config))
	o.Set("enabled", w.Enabled)
	o.Set("next_poll_at", isoOrNil(w.NextPollAt))
	o.Set("last_polled_at", isoOrNil(w.LastPolledAt))
	if w.LastError == nil {
		o.Set("last_error", nil)
	} else {
		o.Set("last_error", *w.LastError)
	}
	o.Set("consecutive_failures", int64(w.ConsecutiveFailures))
	o.Set("cursor", orEmptyObj(w.Cursor))
	o.Set("created_at", isoOrNil(w.CreatedAt))
	if w.Kind != "account_group" {
		return o
	}
	var accounts []any
	if a, ok := objGet(w.Config, "accounts").([]any); ok {
		accounts = a
	}
	if name := objGet(w.Config, "workbook_name"); truthyStr(name) {
		o.Set("target", name)
	}
	o.Set("workbook_id", objGet(w.Config, "workbook_id"))
	o.Set("scope_key", objGet(w.Config, "scope_key"))
	o.Set("account_count", int64(len(accounts)))
	ids := []any{}
	for _, it := range accounts {
		if id := objGet(it, "account_id"); truthyStr(id) {
			ids = append(ids, id)
		}
	}
	o.Set("account_ids", ids)
	hasErr := w.LastError != nil && *w.LastError != ""
	switch {
	case !w.Enabled:
		o.Set("state", "paused")
	case hasErr:
		o.Set("state", "degraded")
	default:
		o.Set("state", "active")
	}
	o.Set("attempt_count", pyIntOrZero(objGet(w.Cursor, "attempt_count")))
	if w.LastError == nil {
		o.Set("last_error_class", nil)
	} else {
		o.Set("last_error_class", *w.LastError)
	}
	if hasErr && w.Enabled && w.NextPollAt != nil {
		o.Set("next_retry_at", autosvc.Iso(*w.NextPollAt))
	} else {
		o.Set("next_retry_at", nil)
	}
	o.Set("collector_health", orEmptyObj(objGet(w.Cursor, "collector_health")))
	retry := wbcrud.NewObj()
	retry.Set("method", "POST")
	retry.Set("url", "/api/watches/"+w.ID+"/poll")
	retry.Set("label", "Retry collectors now")
	o.Set("manual_retry_action", retry)
	return o
}

// MarshalJSON is WatchSubscription _to_api.
func (w Watch) MarshalJSON() ([]byte, error) { return w.Obj().MarshalJSON() }

// Created is the POST /watches result: _to_api plus webhook_rule_id.
type Created struct {
	Watch
	WebhookRuleID *string
}

// MarshalJSON appends webhook_rule_id after the _to_api keys.
func (c Created) MarshalJSON() ([]byte, error) {
	o := c.Watch.Obj()
	if c.WebhookRuleID == nil {
		o.Set("webhook_rule_id", nil)
	} else {
		o.Set("webhook_rule_id", *c.WebhookRuleID)
	}
	return o.MarshalJSON()
}
