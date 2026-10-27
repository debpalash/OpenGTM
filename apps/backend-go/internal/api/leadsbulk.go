package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// enqueueJob is the queue seam (tests swap it to force an enqueue failure).
var enqueueJob = queue.Enqueue

// maxBulkEnrichIDs is the per-call cap (routers/leads.py: list(dict.fromkeys(ids))[:100]).
const maxBulkEnrichIDs = 100

// bulkEnrichPayload mirrors the dict handed to add_job; field order is the Python key order.
type bulkEnrichPayload struct {
	JobID       string     `json:"job_id"`
	IDs         []*big.Int `json:"ids"`
	Action      string     `json:"action"`
	WorkspaceID string     `json:"workspace_id"`
	Slug        string     `json:"slug"`
}

type bulkEnrichResult struct {
	OK     bool   `json:"ok"`
	JobID  string `json:"job_id"`
	Count  int    `json:"count"`
	Action string `json:"action"`
}

// registerBulk registers POST /api/leads/bulk-enrich (require_editor). It only enqueues a
// bulk_enrich job for the Python worker; enrichment itself stays in Python. The route has no
// @limiter in Python, so no rate limit here.
func registerBulk(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := bulkRoutes{d: d}
	r.Handle("POST /api/leads/bulk-enrich", http.HandlerFunc(h.enrich),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin"))
}

type bulkRoutes struct{ d Deps }

var bulkIntString = regexp.MustCompile(`^[+-]?[0-9]+(_[0-9]+)*(\.0+)?$`)

func intErr(typ, msg string, loc []any, in any) []httpx.FieldError {
	return []httpx.FieldError{{Type: typ, Loc: loc, Msg: msg, Input: in}}
}

// pydanticInt coerces one JSON value like pydantic v2 lax `int` (bool, integral float, numeric
// string accepted); magnitude beyond int4 answers a 422 range error.
func pydanticInt(v any, loc []any) (*big.Int, []httpx.FieldError) {
	const parseMsg = "Input should be a valid integer, unable to parse string as an integer"
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			if tooWide(s) {
				return nil, idRangeErr(loc)
			}
			n, _ := new(big.Int).SetString(s, 10)
			if !fitsInt4(n) {
				return nil, idRangeErr(loc)
			}
			return n, nil
		}
		f, _, err := big.ParseFloat(s, 10, 2000, big.ToNearestEven)
		if err != nil {
			return nil, intErr("int_parsing", parseMsg, loc, nil)
		}
		if ff, _ := f.Float64(); math.IsInf(ff, 0) {
			// Python reports finite_number with input Infinity (not representable in JSON); deviation: input null.
			return nil, intErr("finite_number", "Input should be a finite number", loc, nil)
		}
		if !f.IsInt() {
			return nil, intErr("int_from_float", "Input should be a valid integer, got a number with a fractional part", loc, v)
		}
		n, _ := f.Int(nil)
		if !fitsInt4(n) {
			return nil, idRangeErr(loc)
		}
		return n, nil
	case bool:
		if t {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case string:
		s := strings.TrimSpace(t)
		if !pyIntOK(s) {
			return nil, intErr("int_parsing", parseMsg, loc, v)
		}
		if i := strings.IndexByte(s, '.'); i >= 0 {
			s = s[:i]
		}
		if tooWide(s) {
			return nil, idRangeErr(loc)
		}
		n, _ := new(big.Int).SetString(strings.ReplaceAll(s, "_", ""), 10)
		if !fitsInt4(n) {
			return nil, idRangeErr(loc)
		}
		return n, nil
	}
	return nil, intErr("int_type", "Input should be a valid integer", loc, v)
}

// idRangeErr is the 422 for a lead id that no int4 column can hold (hardening: Python enqueues
// it verbatim and the worker's Postgres lookup then 500s). The input is not echoed.
func idRangeErr(loc []any) []httpx.FieldError {
	return intErr("value_error", "Value error, lead id out of range", loc, nil)
}

// tooWide reports whether a decimal digit string cannot fit int4, without parsing it (so a
// multi-megabyte digit string costs nothing).
func tooWide(s string) bool {
	s = strings.TrimLeft(strings.TrimLeft(s, "+-"), "0_")
	return len(strings.ReplaceAll(s, "_", "")) > 10
}

func fitsInt4(n *big.Int) bool {
	return n.IsInt64() && n.Int64() >= math.MinInt32 && n.Int64() <= math.MaxInt32
}

func pyIntOK(s string) bool { return bulkIntString.MatchString(s) }

func (h bulkRoutes) enrich(w http.ResponseWriter, r *http.Request) {
	body, errs, ok := readLeadBody(w, r)
	if !ok {
		return
	}
	obj, errs := modelBody(body, errs)
	var ids []*big.Int
	action := "find_emails"
	if errs == nil {
		switch raw, present := obj["lead_ids"]; {
		case !present:
			errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", "lead_ids"}, Msg: "Field required", Input: obj})
		default:
			list, isList := raw.([]any)
			if !isList {
				errs = append(errs, httpx.FieldError{Type: "list_type", Loc: []any{"body", "lead_ids"}, Msg: "Input should be a valid list", Input: raw})
				break
			}
			ids = make([]*big.Int, 0, len(list))
			for i, el := range list {
				n, e := pydanticInt(el, []any{"body", "lead_ids", i})
				errs = append(errs, e...)
				ids = append(ids, n)
			}
		}
		action = fieldString(obj, "action", false, "find_emails", &errs)
	}
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}

	seen := make(map[string]bool, len(ids))
	uniq := make([]*big.Int, 0, maxBulkEnrichIDs)
	for _, n := range ids {
		if k := n.String(); !seen[k] {
			seen[k] = true
			uniq = append(uniq, n)
		}
	}
	if len(uniq) > maxBulkEnrichIDs {
		uniq = uniq[:maxBulkEnrichIDs]
	}
	if len(uniq) == 0 {
		httpx.Detail(w, http.StatusBadRequest, "lead_ids required")
		return
	}
	if action != "find_emails" && action != "scrape_website" {
		httpx.Detail(w, http.StatusBadRequest, "action must be 'find_emails' or 'scrape_website'")
		return
	}

	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // uuid4 version and variant bits, as uuid.uuid4().hex
	b[8] = b[8]&0x3f | 0x80
	jobID := hex.EncodeToString(b[:])

	wc, _ := identity.WorkspaceFrom(r.Context())
	enqueue := enqueueJob
	if config.BulkEnrichExecutor(h.d.getenv(), wc.WorkspaceID) == "go" { // plan 15 WP-COLLECT cohort
		enqueue = func(ctx context.Context, q gen.DBTX, jobType, ws string, payload any, fireKey string) (int64, error) {
			return collect.Enqueue(ctx, q, jobType, ws, "go", payload, fireKey)
		}
	}
	_, err := enqueue(r.Context(), h.d.Pool, "bulk_enrich", wc.WorkspaceID, bulkEnrichPayload{
		JobID: jobID, IDs: uniq, Action: action, WorkspaceID: wc.WorkspaceID, Slug: wc.Slug,
	}, "")
	if err != nil {
		l := h.d.Log
		if l == nil {
			l = slog.Default()
		}
		l.Error("leads: bulk-enrich enqueue failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bulkEnrichResult{OK: true, JobID: jobID, Count: len(uniq), Action: action})
}
