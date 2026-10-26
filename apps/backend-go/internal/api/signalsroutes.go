package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signals"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const maxSignalsBody = 4 << 20

// signalTypes is SIGNAL_TYPES (monitor.py) key -> label, in Python's order.
var signalTypeLabels = [][2]string{
	{"hiring", "Hiring"}, {"funding", "Funding"}, {"tech_change", "Tech Change"}, {"website_change", "Website Update"},
	{"news", "News Mention"}, {"growth", "Growth Signal"}, {"job_change", "Job Change"}, {"social_activity", "Social Activity"},
}

type orderedLabels [][2]string

func (o orderedLabels) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv[0])
		v, _ := json.Marshal(kv[1])
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type signalsRoutes struct{ d Deps }

// registerSignals registers the signal routes. GET /api/signals/sources stays on FastAPI.
func registerSignals(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := signalsRoutes{d: d}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("signals.write", "editor", "admin")
	r.Handle("GET /api/signals", http.HandlerFunc(h.list), authn, workspace)
	r.Handle("GET /api/signals/counts", http.HandlerFunc(h.counts), authn, workspace)
	r.Handle("GET /api/signals/analytics", http.HandlerFunc(h.analytics), authn, workspace)
	r.Handle("POST /api/signals/mark-read", http.HandlerFunc(h.markRead), authn, workspace, editor)
	r.Handle("POST /api/signals/scan", http.HandlerFunc(h.scan), authn, identity.RequireGlobalAdmin())
}

func (h signalsRoutes) fail(w http.ResponseWriter, what string, err error) {
	log := h.d.Log
	if log == nil {
		log = slog.Default()
	}
	log.Error("signals: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h signalsRoutes) inTx(w http.ResponseWriter, r *http.Request, what string, fn func(pgx.Tx) error) (ok bool, err error) {
	wc, _ := identity.WorkspaceFrom(r.Context())
	if err := tenancy.InWorkspace(r.Context(), h.d.Pool, wc.WorkspaceID, fn); err != nil {
		if errors.Is(err, signals.ErrBadCursor) {
			return false, err
		}
		h.fail(w, what, err)
		return false, err
	}
	return true, nil
}

// listWithOffset is the body when no cursor was passed; listWithCursor when one was.
// Python builds {**page, ...} so the page's key order decides the first three keys.
type listWithOffset struct {
	Signals     []signals.Signal     `json:"signals"`
	HasMore     bool                 `json:"has_more"`
	NextCursor  *string              `json:"next_cursor"`
	Limit       int64                `json:"limit"`
	Offset      int64                `json:"offset"`
	Counts      signals.CountsResult `json:"counts"`
	SignalTypes orderedLabels        `json:"signal_types"`
}

type listWithCursor struct {
	Signals     []signals.Signal     `json:"signals"`
	NextCursor  *string              `json:"next_cursor"`
	HasMore     bool                 `json:"has_more"`
	Limit       int64                `json:"limit"`
	Offset      *int64               `json:"offset"`
	Counts      signals.CountsResult `json:"counts"`
	SignalTypes orderedLabels        `json:"signal_types"`
}


func (h signalsRoutes) list(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	signalType := v.str("signal_type")
	leadID := v.optInt("lead_id")
	limit := v.integer("limit", 50, i64(1), i64(200))
	offset := v.integer("offset", 0, i64(0), nil)
	cursor, _ := v.text("cursor", 1024)
	if v.fail(w) {
		return
	}
	var lead *int64
	if leadID != nil {
		n := int64(*leadID)
		lead = &n
	}
	var (
		page   signals.PageResult
		counts signals.CountsResult
	)
	ok, err := h.inTx(w, r, "list", func(tx pgx.Tx) (err error) {
		if cursor != "" {
			var types []string
			if signalType != "" {
				types = []string{signalType}
			}
			if page, err = signals.Page(r.Context(), tx, types, lead, int(limit), cursor); err != nil {
				return err
			}
		} else {
			rows, err := signals.List(r.Context(), tx, signalType, lead, int(limit)+1, int(offset))
			if err != nil {
				return err
			}
			page.HasMore = len(rows) > int(limit)
			if page.HasMore {
				rows = rows[:limit]
				last := rows[len(rows)-1]
				c := nextCursor(last)
				page.NextCursor = &c
			}
			page.Signals = rows
		}
		counts, err = signals.Counts(r.Context(), tx)
		return err
	})
	if !ok {
		if errors.Is(err, signals.ErrBadCursor) {
			httpx.Detail(w, http.StatusUnprocessableEntity, "invalid signal cursor")
		}
		return
	}
	if page.Signals == nil {
		page.Signals = []signals.Signal{}
	}
	labels := orderedLabels(signalTypeLabels)
	if cursor != "" {
		writePy(w, listWithCursor{Signals: page.Signals, NextCursor: page.NextCursor, HasMore: page.HasMore,
			Limit: limit, Offset: nil, Counts: counts, SignalTypes: labels})
		return
	}
	writePy(w, listWithOffset{Signals: page.Signals, HasMore: page.HasMore, NextCursor: page.NextCursor,
		Limit: limit, Offset: offset, Counts: counts, SignalTypes: labels})
}

// nextCursor encodes the keyset cursor of the last returned signal. A NULL created_at renders as
// json.dumps([None, id]) (the next request then answers 422, like Python).
func nextCursor(s signals.Signal) string {
	if s.CreatedAt != nil {
		return signals.EncodeCursor(*s.CreatedAt, s.ID)
	}
	id, _ := json.Marshal(s.ID)
	return base64.RawURLEncoding.EncodeToString(append(append([]byte("[null,"), id...), ']'))
}

func (h signalsRoutes) counts(w http.ResponseWriter, r *http.Request) {
	var c signals.CountsResult
	if ok, _ := h.inTx(w, r, "counts", func(tx pgx.Tx) (err error) {
		c, err = signals.Counts(r.Context(), tx)
		return err
	}); !ok {
		return
	}
	writePy(w, c)
}

func (h signalsRoutes) analytics(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	days := v.integer("days", 30, i64(7), i64(90))
	if v.fail(w) {
		return
	}
	var res signals.AnalyticsResult
	if ok, _ := h.inTx(w, r, "analytics", func(tx pgx.Tx) (err error) {
		res, err = signals.Analytics(r.Context(), tx, int(days), time.Now())
		return err
	}); !ok {
		return
	}
	writePy(w, res)
}

func (h signalsRoutes) markRead(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSignalsBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	obj, errs := decodeBodyObject(raw)
	if errs == nil {
		errs = checkSignalIDs(obj)
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	list := obj["signal_ids"].([]any)
	ids := make([]string, len(list))
	for i, x := range list {
		ids[i] = x.(string)
	}
	if ok, _ := h.inTx(w, r, "mark-read", func(tx pgx.Tx) error {
		return signals.MarkRead(r.Context(), tx, ids)
	}); !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	}{"ok", len(ids)})
}

// checkSignalIDs validates MarkReadRequest.signal_ids: List[str].
func checkSignalIDs(obj map[string]any) []httpx.FieldError {
	v, present := obj["signal_ids"]
	if !present {
		return []httpx.FieldError{{Type: "missing", Loc: []any{"body", "signal_ids"}, Msg: "Field required", Input: obj}}
	}
	list, isList := v.([]any)
	if !isList {
		return []httpx.FieldError{{Type: "list_type", Loc: []any{"body", "signal_ids"}, Msg: "Input should be a valid list", Input: v}}
	}
	var errs []httpx.FieldError
	for i, x := range list {
		if _, isStr := x.(string); !isStr {
			errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", "signal_ids", i}, Msg: "Input should be a valid string", Input: x})
		}
	}
	return errs
}

func (h signalsRoutes) scan(w http.ResponseWriter, r *http.Request) {
	if _, err := queue.Enqueue(r.Context(), h.d.Pool, "signal_scan", "", map[string]any{}, ""); err != nil {
		h.fail(w, "scan enqueue", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "started"})
}
