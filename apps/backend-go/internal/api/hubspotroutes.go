package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// newHubSpotClient builds the HubSpot client the routes use (tests swap in a fake portal). The
// default client is the SSRF-guarded egress client with Python's timeouts.
var newHubSpotClient = func(r *secrets.Resolver) *crm.HubSpot { return &crm.HubSpot{Secrets: r} }

// Bounds Python does not have (plan 07 D4): the limit and the id list are capped.
const (
	hubspotMaxLimit   = 1000
	hubspotMaxLeadIDs = 1000
)

type hubspotRoutes struct {
	hs  *crm.HubSpot
	res *secrets.Resolver
	d   Deps
	log *slog.Logger
}

// registerHubspot registers routers/hubspot.py: status and contacts need a workspace; connect
// and sync need the activation.write permission (editor or admin role).
func registerHubspot(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Envelope == nil {
		return
	}
	res := secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), d.getenv())
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	h := hubspotRoutes{hs: newHubSpotClient(res), res: res, d: d, log: log}
	authn, workspace := identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)
	editor := identity.RequireRole("activation.write", "editor", "admin")
	r.Handle("GET /api/crm/hubspot/status", http.HandlerFunc(h.status), authn, workspace)
	r.Handle("POST /api/crm/hubspot/connect", http.HandlerFunc(h.connect), authn, workspace, editor)
	r.Handle("POST /api/crm/hubspot/sync", http.HandlerFunc(h.sync), authn, workspace, editor)
	r.Handle("GET /api/crm/hubspot/contacts", http.HandlerFunc(h.contacts), authn, workspace)
}

func (h hubspotRoutes) fail(w http.ResponseWriter, what string, err error) {
	// err can wrap a vendor or DB message; only its type is logged (never a secret).
	h.log.Error("hubspot: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h hubspotRoutes) status(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	if !h.hs.IsConnected(r.Context(), wc.WorkspaceID) {
		httpx.WriteJSON(w, http.StatusOK, orderedPairs{{"connected", false}, {"error", "No HubSpot token configured"}})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.hs.TestConnection(r.Context(), wc.WorkspaceID))
}

func (h hubspotRoutes) connect(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	obj, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	var errs []httpx.FieldError
	token := fieldString(obj, "token", true, "", &errs)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	// Set trims and ignores a blank token (the saved secret is kept), as Python's set_secret.
	if err := h.res.Set(r.Context(), wc.WorkspaceID, "HUBSPOT_TOKEN", token); err != nil {
		h.fail(w, "save token", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, orderedPairs{{"status", "ok"}, {"message", "HubSpot token saved"}})
}

type syncRequest struct {
	leadIDs []int64
	tier    string
	limit   int
}

// parseSync validates the SyncRequest model: lead_ids Optional[List[int]], tier Optional[str],
// limit int (1..1000 here).
func parseSync(obj map[string]any) (syncRequest, []httpx.FieldError) {
	req := syncRequest{limit: 50}
	var errs []httpx.FieldError
	if v, present := obj["lead_ids"]; present && v != nil {
		list, isList := v.([]any)
		switch {
		case !isList:
			errs = append(errs, httpx.FieldError{Type: "list_type", Loc: []any{"body", "lead_ids"}, Msg: "Input should be a valid list", Input: v})
		case len(list) > hubspotMaxLeadIDs:
			errs = append(errs, httpx.FieldError{Type: "too_long", Loc: []any{"body", "lead_ids"},
				Msg: "List should have at most 1000 items after validation, not " + strconv.Itoa(len(list)), Input: v,
				Ctx: map[string]any{"field_type": "List", "max_length": hubspotMaxLeadIDs, "actual_length": len(list)}})
		default:
			for i, el := range list {
				loc := []any{"body", "lead_ids", i}
				if b, isBool := el.(bool); isBool {
					if b {
						req.leadIDs = append(req.leadIDs, 1)
					} else {
						req.leadIDs = append(req.leadIDs, 0)
					}
					continue
				}
				n, fe := pyInt(el, loc)
				if fe != nil {
					errs = append(errs, httpx.FieldError{Type: fe.Type, Loc: fe.Loc, Msg: fe.Msg, Input: fe.Input, Ctx: fe.Ctx})
					continue
				}
				if n.huge {
					n.v = -1 // beyond int64: no such lead
				}
				req.leadIDs = append(req.leadIDs, n.v)
			}
		}
	}
	if v, present := obj["tier"]; present && v != nil {
		if s, isStr := v.(string); isStr {
			req.tier = s
		} else {
			errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", "tier"}, Msg: "Input should be a valid string", Input: v})
		}
	}
	if v, present := obj["limit"]; present {
		var n pyIntVal
		var fe *mcpFieldErr
		if b, isBool := v.(bool); isBool {
			if b {
				n.v = 1
			}
		} else {
			n, fe = pyInt(v, []any{"body", "limit"})
		}
		loc := []any{"body", "limit"}
		switch {
		case fe != nil:
			errs = append(errs, httpx.FieldError{Type: fe.Type, Loc: fe.Loc, Msg: fe.Msg, Input: fe.Input, Ctx: fe.Ctx})
		case n.huge || n.v > hubspotMaxLimit:
			errs = append(errs, httpx.FieldError{Type: "less_than_equal", Loc: loc, Msg: "Input should be less than or equal to 1000", Input: v, Ctx: map[string]any{"le": hubspotMaxLimit}})
		case n.v < 1:
			errs = append(errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Msg: "Input should be greater than or equal to 1", Input: v, Ctx: map[string]any{"ge": 1}})
		default:
			req.limit = int(n.v)
		}
	}
	return req, errs
}

// syncLeads loads the leads to push in the caller's tenant: the listed ids in request order
// (unknown and foreign ones dropped), else the tier, else the top limit; only with an email.
func syncLeads(ctx context.Context, wc identity.WorkspaceCtx, req syncRequest) ([]crm.Lead, error) {
	var leads []leadsread.Lead
	err := wc.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		switch {
		case len(req.leadIDs) > 0:
			var found []leadsread.Lead
			if found, err = leadsread.LeadsByIDs(ctx, tx, req.leadIDs); err != nil {
				return err
			}
			byID := make(map[int64]leadsread.Lead, len(found))
			for _, l := range found {
				id, _ := l.Get("id")
				if n, ok := id.(int64); ok {
					byID[n] = l
				}
			}
			for _, id := range req.leadIDs {
				if l, ok := byID[id]; ok {
					leads = append(leads, l)
				}
			}
		default:
			leads, err = leadsread.ListLeads(ctx, tx, leadsread.ListParams{ScoreTier: req.tier, Limit: req.limit, OrderBy: "score DESC"})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]crm.Lead, 0, len(leads))
	for _, l := range leads {
		if e, _ := l.Get("email"); e != nil && e != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func (h hubspotRoutes) sync(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	obj, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	req, errs := parseSync(obj)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	if !h.hs.IsConnected(r.Context(), wc.WorkspaceID) {
		httpx.Detail(w, http.StatusBadRequest, "HubSpot not connected. Add token first.")
		return
	}
	leads, err := syncLeads(r.Context(), wc, req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		h.fail(w, "read leads", err)
		return
	}
	if len(leads) == 0 {
		httpx.WriteJSON(w, http.StatusOK, orderedPairs{{"created", 0}, {"updated", 0}, {"failed", 0}, {"message", "No leads with emails to sync"}})
		return
	}
	res := h.hs.PushLeadsBatch(r.Context(), wc.WorkspaceID, leads, nil)
	if r.Context().Err() != nil {
		return // the client went away; the batch stopped pushing
	}
	errsOut := res.Errors
	if len(errsOut) > 5 {
		errsOut = errsOut[:5]
	}
	if errsOut == nil {
		errsOut = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, orderedPairs{{"created", res.Created}, {"updated", res.Updated}, {"failed", res.Failed},
		{"errors", errsOut}, {"total_synced", res.Created + res.Updated}})
}

func (h hubspotRoutes) contacts(w http.ResponseWriter, r *http.Request) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	limit, ok := queryInt(w, r, "limit", 20, intp(1), intp(hubspotMaxLimit))
	if !ok {
		return
	}
	if !h.hs.IsConnected(r.Context(), wc.WorkspaceID) {
		httpx.Detail(w, http.StatusBadRequest, "HubSpot not connected")
		return
	}
	contacts, err := h.hs.GetContacts(r.Context(), wc.WorkspaceID, limit)
	if err != nil {
		return // cancelled
	}
	httpx.WriteJSON(w, http.StatusOK, orderedPairs{{"contacts", contacts}, {"count", len(contacts)}})
}

// orderedPairs marshals as a JSON object in the given key order (Python dict order).
type orderedPairs []struct {
	K string
	V any
}

func (o orderedPairs) MarshalJSON() ([]byte, error) {
	b := []byte{'{'}
	for i, p := range o {
		if i > 0 {
			b = append(b, ',')
		}
		k, err := marshalNoEscape(p.K)
		if err != nil {
			return nil, err
		}
		v, err := marshalNoEscape(p.V)
		if err != nil {
			return nil, err
		}
		b = append(append(append(b, k...), ':'), v...)
	}
	return append(b, '}'), nil
}

// marshalNoEscape is json.Marshal without HTML escaping (Python writes & < > raw).
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
