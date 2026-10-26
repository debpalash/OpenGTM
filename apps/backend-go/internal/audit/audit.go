// Package audit records governance audit events for state-changing requests,
// mirroring legacy/python/apps/api/services/governance/audit.py.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const writeTimeout = 5 * time.Second

var sensitiveKey = regexp.MustCompile(`(?i)token|secret|password|authorization|cookie|credential`)

// holder is the request-scoped record created by Middleware. Per-route
// middleware (identity.Workspace) runs inside the global chain and cannot
// return values outward through the context, so it writes here instead.
type holder struct {
	mu          sync.Mutex
	actorUserID int32
	actorRole   string
	workspaceID string
	metadata    map[string]any
}

type holderKey struct{}

func from(ctx context.Context) *holder {
	h, _ := ctx.Value(holderKey{}).(*holder)
	return h
}

// SetActor records who acted and in which workspace. No-op without Middleware.
func SetActor(ctx context.Context, userID int32, role, workspaceID string) {
	if h := from(ctx); h != nil {
		h.mu.Lock()
		h.actorUserID, h.actorRole, h.workspaceID = userID, role, workspaceID
		h.mu.Unlock()
	}
}

// SetMetadata attaches endpoint-supplied audit context (sanitised on write).
func SetMetadata(ctx context.Context, md map[string]any) {
	if h := from(ctx); h != nil {
		h.mu.Lock()
		h.metadata = md
		h.mu.Unlock()
	}
}

// SafeMetadata bounds endpoint-supplied context and drops secret-looking keys.
func SafeMetadata(v any, depth int) any {
	if depth > 3 {
		return "[truncated]"
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic subset when oversized
		out := map[string]any{}
		for _, k := range keys[:min(len(keys), 50)] {
			if sensitiveKey.MatchString(k) {
				continue
			}
			out[cut(k, 64)] = SafeMetadata(x[k], depth+1)
		}
		return out
	case []any:
		out := make([]any, 0, min(len(x), 100))
		for _, item := range x[:min(len(x), 100)] {
			out = append(out, SafeMetadata(item, depth+1))
		}
		return out
	case []string:
		out := make([]any, 0, min(len(x), 100))
		for _, item := range x[:min(len(x), 100)] {
			out = append(out, SafeMetadata(item, depth+1))
		}
		return out
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return x
	case string:
		return cut(x, 500)
	default:
		return normalise(x, depth)
	}
}

// normalise converts typed maps, slices, arrays, pointers and structs into the
// generic forms handled above so redaction and caps apply at every level.
func normalise(x any, depth int) any {
	rv := reflect.ValueOf(x)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return SafeMetadata(rv.Elem().Interface(), depth)
	case reflect.Map:
		m := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			m[fmt.Sprint(it.Key().Interface())] = it.Value().Interface()
		}
		return SafeMetadata(m, depth)
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			break // bytes: stringified like Python's str(bytes)
		}
		n := min(rv.Len(), 100)
		l := make([]any, n)
		for i := range l {
			l[i] = rv.Index(i).Interface()
		}
		return SafeMetadata(l, depth)
	case reflect.Struct:
		if raw, err := json.Marshal(x); err == nil {
			var g any
			if json.Unmarshal(raw, &g) == nil {
				return SafeMetadata(g, depth)
			}
		}
	}
	return cut(fmt.Sprint(x), 500)
}

func cut(s string, n int) string {
	if len(s) <= n { // bytes >= runes, so this is a safe fast path
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Middleware must be registered globally, after RequestID. It creates the
// request-scoped holder that identity.Workspace fills via SetActor, runs the
// handler, then records write requests that resolved a workspace.
func Middleware(pool *pgxpool.Pool, log *slog.Logger) httpx.Middleware {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := &holder{}
			req := r.WithContext(context.WithValue(r.Context(), holderKey{}, h))
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				rec := recover()
				status := sw.status
				if rec != nil {
					status = http.StatusInternalServerError
				}
				func() {
					defer func() { // a failing record must not mask the handler's panic
						if e := recover(); e != nil {
							log.Error("governance audit record panicked")
						}
					}()
					record(pool, log, req, h, status)
				}()
				if rec != nil {
					panic(rec)
				}
			}()
			next.ServeHTTP(sw, req)
		})
	}
}

func isWrite(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodPatch || m == http.MethodDelete
}

func outcome(status int) string {
	switch {
	case status < 400:
		return "success"
	case status == 401 || status == 403:
		return "denied"
	}
	return "error"
}

func record(pool *pgxpool.Pool, log *slog.Logger, r *http.Request, h *holder, status int) {
	if !isWrite(r.Method) {
		return
	}
	h.mu.Lock()
	ws, uid, role, md := h.workspaceID, h.actorUserID, h.actorRole, h.metadata
	h.mu.Unlock()
	if ws == "" || pool == nil {
		return
	}
	reqID := httpx.RequestIDFrom(r.Context())
	// r.Pattern is set by ServeMux on the request value it was handed (r).
	route := r.URL.Path
	if p := r.Pattern; p != "" {
		if _, after, ok := strings.Cut(p, " "); ok {
			p = after
		}
		route = p
	}
	if md == nil {
		md = map[string]any{}
	}
	mdJSON, err := json.Marshal(SafeMetadata(md, 0))
	if err != nil {
		mdJSON = []byte("{}")
	}
	arg := gen.InsertGovernanceAuditParams{
		ID:             newUUID4(),
		WorkspaceID:    ws,
		ActorRole:      role,
		Method:         r.Method,
		Route:          cut(route, 255),
		ResourcePath:   cut(r.URL.Path, 500),
		ResponseStatus: int32(status),
		Outcome:        outcome(status),
		RequestID:      cut(reqID, 64),
		MetadataJson:   mdJSON,
	}
	if uid > 0 {
		arg.ActorUserID = pgtype.Int4{Int32: uid, Valid: true}
	}
	// Detach from the request: the client may already be gone.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), writeTimeout)
	defer cancel()
	err = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		return gen.New(tx).InsertGovernanceAudit(ctx, arg)
	})
	if err != nil {
		log.Error("governance audit write failed", "request_id", reqID, "error", err.Error())
	}
}

func newUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
