package autoengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/safecond"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// ErrPreviewNoTrigger is a preview of a rule that does not exist in the workspace.
var ErrPreviewNoTrigger = errors.New("autoengine: trigger not found")

// PreviewLimitDefault and PreviewLimitMax bound the sample (Python:
// max(1, min(body.limit or 25, 100))).
const (
	PreviewLimitDefault = 25
	PreviewLimitMax     = 100
)

// ClampPreviewLimit is `max(1, min(limit or 25, 100))`: 0 is the default.
func ClampPreviewLimit(limit int64) int {
	if limit == 0 {
		limit = PreviewLimitDefault
	}
	if limit > PreviewLimitMax {
		limit = PreviewLimitMax
	}
	if limit < 1 {
		limit = 1
	}
	return int(limit)
}

// Preview is POST /triggers/{id}/preview: the rule evaluated against its candidate
// rows with NO write, NO debit and NO outbound call (it only reads through tx and
// resolves templates in memory). rowIDs are the request's row ids; ids that are not
// integers or not rows of the workspace are dropped (D4: Python 500s on non-numeric).
// tx must be inside tenancy.InWorkspace; the caller makes it read-only.
func Preview(ctx context.Context, tx pgx.Tx, ws, triggerID string, rowIDs []string, limit, maxRows int, now time.Time) (*wbcrud.Obj, error) {
	trig, err := gen.New(tx).AutoRunTrigger(ctx, gen.AutoRunTriggerParams{ID: triggerID, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPreviewNoTrigger
	}
	if err != nil {
		return nil, err
	}
	if maxRows < 1 {
		maxRows = 500
	}
	var targets any
	if len(rowIDs) > 0 {
		ts, err := (&autosvc.Service{}).PreviewTargets(ctx, tx, ws, rowIDs)
		if err != nil {
			return nil, err
		}
		list := make([]any, len(ts))
		for i, t := range ts {
			list[i] = map[string]any{"workbook_id": t.WorkbookID, "row_id": t.RowID}
		}
		targets = list
	}
	cands, err := resolveCandidates(ctx, tx, ws, parseScope(trig.ScopeWorkbookIds), targets, maxRows)
	if err != nil {
		return nil, err
	}
	actions := previewActions(trig.Actions)
	cond := ""
	if trig.Condition.Valid {
		cond = trig.Condition.String
	}

	matched, projected := 0, 0.0
	sample := []any{}
	for _, cand := range cands {
		cells := rowCells(cand.row)
		if !safecond.EvaluateAt(cond, cells, cand.sc, now).Passed {
			continue
		}
		matched++
		lead := leadDataFor(cand.row)
		previews := make([]any, 0, len(actions))
		for _, a := range actions {
			cost := autoactions.ProjectCost(a.costInput, cand.cols, cand.defaults)
			projected += cost
			item := wbcrud.NewObj()
			item.Set("type", a.typ)
			item.Set("would_charge_usd", money(cost))
			item.Set("resolved_payload_preview", previewPayload(a, lead, cand.cols))
			previews = append(previews, item)
		}
		if len(sample) < limit {
			s := wbcrud.NewObj()
			rid, _ := strconv.ParseInt(cand.rowID, 10, 64)
			s.Set("row_id", strconv.FormatInt(rid, 10))
			s.Set("workbook_id", cand.workbookID)
			s.Set("condition_pass", true)
			s.Set("actions", previews)
			sample = append(sample, s)
		}
	}
	out := wbcrud.NewObj()
	out.Set("matched", matched)
	out.Set("sample", sample)
	out.Set("projected_total_usd", money(projected))
	return out, nil
}

// previewAction is one stored action in key order. typ is the raw `type` value
// (null for a missing type or a non-object action, D4).
type previewAction struct {
	typ       any
	cfg       colgraph.Col
	costInput map[string]any
}

func previewActions(raw []byte) []previewAction {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]previewAction, 0, len(items))
	for _, it := range items {
		pa := previewAction{costInput: map[string]any{}}
		if v, err := rowtmpl.DecodeOrdered(it); err == nil {
			if c, ok := v.(colgraph.Col); ok {
				pa.typ = c.Get("type")
				if cfg, ok := c.Get("config").(colgraph.Col); ok {
					pa.cfg = cfg
				}
				pa.costInput["type"] = c.Get("type")
				pa.costInput["config"] = c.Get("config")
			}
		}
		if pa.cfg.Vals == nil {
			pa.cfg = colgraph.Col{Vals: map[string]any{}}
		}
		out = append(out, pa)
	}
	return out
}

// money is round(x, 4) as a Python-repr float.
func money(f float64) json.Number {
	if !math.IsNaN(f) && !math.IsInf(f, 0) {
		if r, err := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 4, 64), 64); err == nil {
			f = r
		}
	}
	s, err := canonjson.FloatRepr(f)
	if err != nil {
		return json.Number("0.0")
	}
	return json.Number(s)
}

func falsy(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case json.Number:
		f, err := t.Float64()
		return err == nil && f == 0
	case []any:
		return len(t) == 0
	case colgraph.Col:
		return len(t.Keys) == 0
	}
	return false
}

// previewPayload is _preview_payload: the resolved request a run would send, with
// a template failure rendered as {"error": "preview failed: <e>"} (the action
// config is echoed only through templates; header_secret_ref is never read).
func previewPayload(a previewAction, lead map[string]any, cols []autoactions.Column) any {
	t, _ := a.typ.(string)
	switch t {
	case "webhook":
		url := ""
		if v := a.cfg.Get("url"); !falsy(v) {
			s, ok := v.(string)
			if !ok {
				return previewErr(fmt.Errorf("expected string or bytes-like object"))
			}
			r, err := rowtmpl.Resolve(s, lead, cols)
			if err != nil {
				return previewErr(err)
			}
			url = r
		}
		var body any
		switch b := a.cfg.Get("body").(type) {
		case colgraph.Col, []any:
			r, err := rowtmpl.ResolveDeep(b, lead, cols)
			if err != nil {
				return previewErr(err)
			}
			body = r
		default:
			if !falsy(b) {
				r, err := rowtmpl.Resolve(rowtmpl.PyStr(b), lead, cols)
				if err != nil {
					return previewErr(err)
				}
				body = r
			}
		}
		method := "POST"
		if m := a.cfg.Get("method"); !falsy(m) {
			s, ok := m.(string)
			if !ok {
				return previewErr(fmt.Errorf("object has no attribute 'upper'"))
			}
			method = strings.ToUpper(s)
		}
		o := wbcrud.NewObj()
		o.Set("url", url)
		o.Set("method", method)
		o.Set("body", ordered(body))
		return o
	case "push_crm":
		o := wbcrud.NewObj()
		crm := any("hubspot")
		if v, ok := a.cfg.Lookup("type"); ok {
			crm = v
		}
		o.Set("crm", ordered(crm))
		o.Set("field_map", ordered(a.cfg.Get("field_map")))
		return o
	case "re_enrich":
		o := wbcrud.NewObj()
		ids := any([]any{})
		if v, ok := a.cfg.Lookup("column_ids"); ok {
			ids = v
		}
		o.Set("column_ids", ordered(ids))
		return o
	}
	return wbcrud.NewObj()
}

// MaskWebhookURLs masks every webhook preview URL in a Preview result in place, for a
// caller without automations.manage (final review M2, S-L3): the URL keeps only
// scheme://host, as GET and list show the stored rule config (autosvc Trigger.Masked).
// The preview never renders headers, so there is nothing else to mask.
func MaskWebhookURLs(out *wbcrud.Obj) {
	if out == nil {
		return
	}
	sample, _ := out.Get("sample")
	items, _ := sample.([]any)
	for _, it := range items {
		s, ok := it.(*wbcrud.Obj)
		if !ok {
			continue
		}
		acts, _ := s.Get("actions")
		list, _ := acts.([]any)
		for _, a := range list {
			act, ok := a.(*wbcrud.Obj)
			if !ok {
				continue
			}
			if typ, _ := act.Get("type"); typ != "webhook" {
				continue
			}
			pv, _ := act.Get("resolved_payload_preview")
			p, ok := pv.(*wbcrud.Obj)
			if !ok {
				continue
			}
			if u, ok := p.Get("url"); ok {
				if str, isStr := u.(string); isStr && str != "" {
					p.Set("url", maskURL(str))
				}
			}
		}
	}
}

// maskURL keeps scheme://authority (userinfo dropped) and hides any path, query or
// fragment; the same rule as autosvc's GET/list masking.
func maskURL(u string) string {
	const masked = "***"
	sep := strings.Index(u, "://")
	if sep < 0 {
		return masked
	}
	rest := u[sep+3:]
	end := len(rest)
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		end = j
	}
	auth := rest[:end]
	if at := strings.LastIndexByte(auth, '@'); at >= 0 {
		auth = auth[at+1:]
	}
	if end == len(rest) {
		return u[:sep+3] + auth
	}
	return u[:sep+3] + auth + "/" + masked
}

func previewErr(err error) any {
	o := wbcrud.NewObj()
	o.Set("error", "preview failed: "+err.Error())
	return o
}

// ordered turns decoded ordered objects (colgraph.Col) into wbcrud.Obj so the
// response keeps the stored key order.
func ordered(v any) any {
	switch t := v.(type) {
	case colgraph.Col:
		o := wbcrud.NewObj()
		for _, k := range t.Keys {
			o.Set(k, ordered(t.Vals[k]))
		}
		return o
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = ordered(e)
		}
		return out
	}
	return v
}
