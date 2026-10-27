package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectintent"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// maxCollectBody caps the request body of POST /api/collect (Python has no cap; the query is limited to
// COLLECT_QUERY_MAX_CHARS characters, so anything near this size is not a legitimate request).
const maxCollectBody = 64 << 10

// Test seams. nil means the production behaviour.
var (
	collectNewID   func() string                                                                               // uuid4().hex
	collectEnqueue func(ctx context.Context, tx pgx.Tx, ws string, payload any, fireKey string) (int64, error) // queue insert in tx
	collectBus     func(d Deps) *leadprogress.Bus                                                              // progress bus
)

// collectEnqueueDefault enqueues the collect job (priority 1, max_retries 3, fire key set), as
// queue_service.add_job does, for the executor the cohort names: 'go' only for workspaces in the
// GO_EXECUTOR_ENABLED + GO_EXECUTOR_WORKSPACES cohort with GO_EXECUTOR_COLLECT on (plan 15
// WP-COLLECT), else 'python'.
func collectEnqueueDefault(ctx context.Context, tx pgx.Tx, ws string, payload any, fireKey string) (int64, error) {
	return collect.Enqueue(ctx, tx, "collect", ws, config.CollectExecutor(config.Getenv, ws), payload, fireKey)
}

// collectPayload is the queue payload in Python's dict order.
type collectPayload struct {
	JobID       string `json:"job_id"`
	Query       string `json:"query"`
	Intent      string `json:"intent"`
	WorkspaceID string `json:"workspace_id"`
	Slug        string `json:"slug"`
}

// collectResponse is the 200 body in Python's dict order.
type collectResponse struct {
	OK          bool   `json:"ok"`
	JobID       string `json:"job_id"`
	QueueJobID  int64  `json:"queue_job_id"`
	Query       string `json:"query"`
	Intent      string `json:"intent"`
	WorkspaceID string `json:"workspace_id"`
}

type collectBody struct {
	Query  string
	Intent *string
}

type collectCtxKey struct{}

var errCollectQueue = errors.New("collect: enqueue failed")

var (
	defaultBusOnce sync.Once
	defaultBus     *leadprogress.Bus
)

// progressBus is the process-wide leadgen progress bus on d.Redis (nil Redis: in-process deque only).
func progressBus(d Deps) *leadprogress.Bus {
	if collectBus != nil {
		return collectBus(d)
	}
	defaultBusOnce.Do(func() { defaultBus = leadprogress.NewBus(d.Redis) })
	return defaultBus
}

type collectRoutes struct {
	d        Deps
	maxChars int
}

// registerCollectRoute registers POST /api/collect (routers/leads.py start_collection): require_editor
// (tables.write; owner, admin, editor), 20/minute per workspace key.
//
// slowapi's limit runs inside the route function, after FastAPI validated the body: a 422 never counts
// against the budget, a 400 does. The same order here: the outer handler validates, then the limited
// handler runs.
func registerCollectRoute(r *httpx.Router, d Deps) {
	h := &collectRoutes{d: d, maxChars: d.Flags.Leadgen.CollectQueryMaxChars}
	if h.maxChars <= 0 {
		h.maxChars = 1000
	}
	var inner http.Handler = http.HandlerFunc(h.start)
	if d.Limiter != nil {
		inner = d.Limiter.Limit("20/minute")(inner)
	}
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := h.parse(w, r)
		if !ok {
			return
		}
		inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), collectCtxKey{}, body)))
	})
	r.Handle("POST /api/collect", outer, identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity),
		identity.RequireRole("tables.write", "editor", "admin"))
}

// parse validates the CollectRequest model: query: str (required), intent: Optional[str], workspace_id: str = "".
// workspace_id is validated and then ignored (tenant identity comes from the authenticated context).
func (h *collectRoutes) parse(w http.ResponseWriter, r *http.Request) (collectBody, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCollectBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return collectBody{}, false
	}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return collectBody{}, false
	}
	var out collectBody
	strErr := func(field string, v any) httpx.FieldError {
		return httpx.FieldError{Type: "string_type", Loc: []any{"body", field}, Msg: "Input should be a valid string", Input: v}
	}
	if v, ok := obj.Get("query"); !ok {
		errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", "query"}, Msg: "Field required", Input: obj})
	} else if s, ok := v.(string); !ok {
		errs = append(errs, strErr("query", v))
	} else if n := utf8.RuneCountInString(s); n > h.maxChars {
		errs = append(errs, httpx.FieldError{Type: "string_too_long", Loc: []any{"body", "query"}, Input: s,
			Msg: fmt.Sprintf("String should have at most %d character%s", h.maxChars, plural(h.maxChars)), Ctx: map[string]any{"max_length": h.maxChars}})
	} else if strings.ContainsRune(s, 0) { // Postgres cannot store NUL; Python answers 500 (audit F12)
		errs = append(errs, httpx.FieldError{Type: "value_error", Loc: []any{"body", "query"}, Input: s,
			Msg: "Value error, string contains a NUL byte"})
	} else {
		out.Query = s
	}
	if v, ok := obj.Get("intent"); ok && v != nil {
		if s, ok := v.(string); !ok {
			errs = append(errs, strErr("intent", v))
		} else if strings.ContainsRune(s, 0) {
			errs = append(errs, httpx.FieldError{Type: "value_error", Loc: []any{"body", "intent"}, Input: s,
				Msg: "Value error, string contains a NUL byte"})
		} else {
			out.Intent = &s
		}
	}
	if v, ok := obj.Get("workspace_id"); ok {
		if _, isStr := v.(string); !isStr {
			errs = append(errs, strErr("workspace_id", v))
		}
	}
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return collectBody{}, false
	}
	return out, true
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func newCollectJobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // uuid4
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

func (h *collectRoutes) start(w http.ResponseWriter, r *http.Request) {
	body, _ := r.Context().Value(collectCtxKey{}).(collectBody)
	query := collectintent.PyStrip(body.Query)
	if query == "" {
		httpx.Detail(w, http.StatusBadRequest, "query is required")
		return
	}
	decision := collectintent.Decide(query, body.Intent)
	if decision.ClarificationRequired {
		// A successful interpretation, not an error: no ledger row and no queue row exist.
		out := wbcrud.NewObj()
		out.Set("ok", false)
		dict := decision.ToDict()
		for _, k := range dict.Keys() {
			v, _ := dict.Get(k)
			out.Set(k, v)
		}
		out.Set("message", decision.Reason)
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}

	wc, _ := identity.WorkspaceFrom(r.Context())
	jobID := newCollectJobID()
	if collectNewID != nil {
		jobID = collectNewID()
	}
	enqueue := collectEnqueueDefault
	if collectEnqueue != nil {
		enqueue = collectEnqueue
	}
	payload := collectPayload{JobID: jobID, Query: query, Intent: decision.Intent, WorkspaceID: wc.WorkspaceID, Slug: wc.Slug}
	var queueID int64
	// The ledger row and the queue row commit together or not at all (Python cannot: two databases, so it
	// compensates with a failed row; here a rolled-back transaction leaves nothing behind).
	err := wc.Tx(r.Context(), func(tx pgx.Tx) error {
		if err := collectledger.Create(r.Context(), tx, wc.WorkspaceID, jobID, query, decision.Intent, collectIntentDetails(decision), time.Now().UTC()); err != nil {
			return err
		}
		id, err := enqueue(r.Context(), tx, wc.WorkspaceID, payload, "collect:"+wc.WorkspaceID+":"+jobID)
		if err != nil {
			h.d.logger().Error("collect: enqueue failed", "err_type", errTypeName(err))
			return errCollectQueue
		}
		queueID = id
		return nil
	})
	if errors.Is(err, errCollectQueue) {
		httpx.Detail(w, http.StatusServiceUnavailable, "Collection queue unavailable")
		return
	}
	if err != nil {
		h.d.logger().Error("collect: create failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// After the commit: a subscriber that sees the event can already read the job. A Redis failure never
	// fails the request (the bus swallows it and keeps the in-process copy).
	ectx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	progressBus(h.d).EmitOrdered(ectx, wc.WorkspaceID, "job_created", leadprogress.Obj{
		{Key: "job_id", Val: jobID},
		{Key: "query", Val: query},
		{Key: "workspace_id", Val: wc.WorkspaceID},
		{Key: "message", Val: "Collection queued: " + query},
	})
	httpx.WriteJSON(w, http.StatusOK, collectResponse{OK: true, JobID: jobID, QueueJobID: queueID, Query: query,
		Intent: decision.Intent, WorkspaceID: wc.WorkspaceID})
}

// collectIntentDetails is json.dumps(decision.to_dict()): default separators (", " and ": "), keys in
// to_dict order, ensure_ascii=True. The jobs routes parse it back with json.loads.
func collectIntentDetails(d collectintent.Decision) string {
	var b bytes.Buffer
	pyDumpsDefault(&b, d.ToDict())
	return b.String()
}

func pyDumpsDefault(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		pyASCIIString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			pyDumpsDefault(b, e)
		}
		b.WriteByte(']')
	case *wbcrud.Obj:
		b.WriteByte('{')
		for i, k := range x.Keys() {
			if i > 0 {
				b.WriteString(", ")
			}
			pyASCIIString(b, k)
			b.WriteString(": ")
			e, _ := x.Get(k)
			pyDumpsDefault(b, e)
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("collect: unsupported decision value %T", v))
	}
}

// pyASCIIString is json.dumps(s) with ensure_ascii=True: non-ASCII as \uXXXX (surrogate pairs above the BMP).
func pyASCIIString(b *bytes.Buffer, s string) {
	const hexd = "0123456789abcdef"
	u := func(c rune) {
		b.WriteString(`\u`)
		b.WriteByte(hexd[c>>12&0xf])
		b.WriteByte(hexd[c>>8&0xf])
		b.WriteByte(hexd[c>>4&0xf])
		b.WriteByte(hexd[c&0xf])
	}
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c < 0x20:
			u(c)
		case c < 0x80:
			b.WriteRune(c)
		case c >= 0x10000:
			c -= 0x10000
			u(0xd800 + c>>10)
			u(0xdc00 + c&0x3ff)
		default:
			u(c)
		}
	}
	b.WriteByte('"')
}
