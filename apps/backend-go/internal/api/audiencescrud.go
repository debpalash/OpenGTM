package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencesched"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Audience CRUD (routers/audiences.py): list, create, patch and delete. Create and a filters patch materialize the
// audience through the refresh engine; create and a refresh_enabled / interval patch (re)schedule it. Like FastAPI the
// order is authentication, workspace, role (403), body validation (422), then the audience lookup (404).

var (
	// audCrudListCap bounds GET /api/audiences (Python returns every row; documented deviation).
	audCrudListCap = 1000
	// audCrudMaxBody bounds the JSON body (Python has no cap; documented deviation).
	audCrudMaxBody int64 = 1 << 20
)

const (
	audDuplicateName = "An audience with this name already exists"
	audNotFound      = "Audience not found"
)

// registerAudiencesCrud registers GET/POST /api/audiences and PATCH/DELETE /api/audiences/{audience_id}.
func registerAudiencesCrud(r *httpx.Router, d Deps) {
	h := audCrud{d: d}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("audiences.write", "editor", "admin")
	r.Handle("GET /api/audiences", http.HandlerFunc(h.list), authn, workspace)
	r.Handle("POST /api/audiences", http.HandlerFunc(h.create), authn, workspace, editor)
	r.Handle("PATCH /api/audiences/{audience_id}", http.HandlerFunc(h.patch), authn, workspace, editor)
	r.Handle("DELETE /api/audiences/{audience_id}", http.HandlerFunc(h.remove), authn, workspace, editor)
}

type audCrud struct{ d Deps }

func (h audCrud) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

func (h audCrud) ws(r *http.Request) identity.WorkspaceCtx {
	wc, _ := identity.WorkspaceFrom(r.Context())
	return wc
}

func (h audCrud) internal(w http.ResponseWriter, what string, err error) {
	h.log().Error("audiences: "+what+" failed", "err_type", fmt.Sprintf("%T", err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// refreshFailed maps a refresh error: the 120 s request bound is the 504 of plan 10 D8, a vanished audience is 404,
// anything else a 500 that never echoes the error text (the health columns already hold it).
func (h audCrud) refreshFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, audiencerefresh.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, audNotFound)
	case errors.Is(err, context.DeadlineExceeded):
		httpx.Detail(w, http.StatusGatewayTimeout, "audience refresh timed out")
	default:
		h.internal(w, "refresh", err)
	}
}

func (h audCrud) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, audCrudMaxBody+1))
	if err != nil || int64(len(raw)) > audCrudMaxBody {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	return raw, true
}

func (h audCrud) invalid(w http.ResponseWriter, err error) {
	var v *audValidation
	if errors.As(err, &v) {
		httpx.ValidationError(w, v.errs...)
		return
	}
	h.internal(w, "decode", err)
}

// audienceID is the path id. Ids are opaque strings; one Postgres cannot store (NUL, invalid UTF-8) matches nothing.
func (h audCrud) audienceID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("audience_id")
	if strings.ContainsRune(id, 0) || !utf8.ValidString(id) {
		httpx.Detail(w, http.StatusNotFound, audNotFound)
		return "", false
	}
	return id, true
}

func audIsUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func audNewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// audISO is a naive datetime as FastAPI renders it: microseconds only when non-zero.
func audISO(t pgtype.Timestamp) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.Format("2006-01-02T15:04:05")
	if us := t.Time.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return &s
}

// audJSON is Audience.to_api(): its key order is the audiencerefresh.Audience field order.
func audJSON(a gen.Audience) audiencerefresh.Audience {
	filters := pyJSONText(string(a.Filters), `{}`) // decode + re-dump like FastAPI; falsy -> {}
	out := audiencerefresh.Audience{ID: a.ID, Name: a.Name, Description: a.Description, Filters: filters, MemberCount: int(a.MemberCount),
		CreatedAt: audISO(a.CreatedAt), UpdatedAt: audISO(a.UpdatedAt), RefreshedAt: audISO(a.RefreshedAt),
		RefreshEnabled: a.RefreshEnabled, RefreshIntervalMinutes: int(a.RefreshIntervalMinutes), NextRefreshAt: audISO(a.NextRefreshAt),
		RefreshHealth: a.RefreshHealth, ConsecutiveRefreshFailures: int(a.ConsecutiveRefreshFailures)}
	if a.LastRefreshError.Valid {
		s := a.LastRefreshError.String
		out.LastRefreshError = &s
	}
	if out.RefreshIntervalMinutes == 0 {
		out.RefreshIntervalMinutes = 60
	}
	if out.RefreshHealth == "" {
		out.RefreshHealth = "unverified"
	}
	return out
}

// refreshExecutor is go_executor.audience_refresh_executor: "go" only with GO_EXECUTOR_ENABLED, GO_EXECUTOR_AUDIENCE_REFRESH
// and the workspace in GO_EXECUTOR_WORKSPACES, read from the environment per call.
func (h audCrud) refreshExecutor(ws string) string {
	c := config.ExecutorCohortFrom(h.d.getenv())
	return automationhook.CohortExecutor(c.Enabled, h.d.Flags.Audiences.GoExecutorAudienceRefresh, c.Workspaces)(ws)
}

// destExecutor is go_executor.destination_executor for workspace ws: "go" only with GO_EXECUTOR_ENABLED, the destination
// type in GO_DEST_TYPES and the workspace in the cohort.
func (h audCrud) destExecutor(ws string) func(string) string {
	c := config.ExecutorCohortFrom(h.d.getenv())
	return func(destType string) string {
		if c.Enabled && config.InCohort(c.Workspaces, ws) && slices.Contains(h.d.Flags.Audiences.GoDestTypes, strings.ToLower(strings.TrimSpace(destType))) {
			return "go"
		}
		return "python"
	}
}

// materialize is materialize_audience (refresh_audience_observed) bounded by the 120 s request timeout.
func (h audCrud) materialize(r *http.Request, ws, id string) error {
	_, err := audiencerefresh.RefreshObserved(r.Context(), h.d.Pool, ws, id, audiencerefresh.Deps{
		AudienceEvents: h.d.AudienceEvents,
		ExecutorFor:    h.destExecutor(ws),
		PageSize:       h.d.Flags.Audiences.RefreshPageSize,
		Timeout:        audiencerefresh.RouteTimeout,
	})
	return err
}

// reschedule is schedule_next in its own workspace transaction.
func (h audCrud) reschedule(r *http.Request, ws string, in audiencesched.ScheduleInput) error {
	return tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		_, err := audiencesched.ScheduleNext(r.Context(), tx, in, time.Now(), h.refreshExecutor(ws))
		return err
	})
}

func (h audCrud) read(r *http.Request, ws, id string) (gen.Audience, error) {
	var a gen.Audience
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		var err error
		a, err = gen.New(tx).AudCrudGet(r.Context(), gen.AudCrudGetParams{ID: id, WorkspaceID: ws})
		return err
	})
	return a, err
}

func (h audCrud) respond(w http.ResponseWriter, r *http.Request, status int, ws, id string) {
	a, err := h.read(r, ws, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Detail(w, http.StatusNotFound, audNotFound)
		return
	}
	if err != nil {
		h.internal(w, "read", err)
		return
	}
	httpx.WriteJSON(w, status, audJSON(a))
}

func (h audCrud) list(w http.ResponseWriter, r *http.Request) {
	ws := h.ws(r).WorkspaceID
	out := []audiencerefresh.Audience{}
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).AudCrudList(r.Context(), gen.AudCrudListParams{WorkspaceID: ws, RowLimit: int32(audCrudListCap)})
		for _, a := range rows {
			out = append(out, audJSON(a))
		}
		return err
	})
	if err != nil {
		h.internal(w, "list", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h audCrud) create(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, err := decodeAudCreate(raw)
	if err != nil {
		h.invalid(w, err)
		return
	}
	ws := h.ws(r).WorkspaceID
	id := audNewID()
	err = tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		return gen.New(tx).AudCrudInsert(r.Context(), gen.AudCrudInsertParams{ID: id, WorkspaceID: ws, Name: in.Name,
			Description: in.Description, Filters: []byte(in.Filters), RefreshEnabled: in.RefreshEnabled, RefreshIntervalMinutes: int32(in.Interval)})
	})
	if audIsUnique(err) {
		httpx.Detail(w, http.StatusConflict, audDuplicateName)
		return
	}
	if err != nil {
		h.internal(w, "create", err)
		return
	}
	if err := h.materialize(r, ws, id); err != nil {
		h.refreshFailed(w, err)
		return
	}
	if err := h.reschedule(r, ws, audiencesched.ScheduleInput{ID: id, WorkspaceID: ws, RefreshEnabled: in.RefreshEnabled, IntervalMinutes: in.Interval}); err != nil {
		h.internal(w, "schedule", err)
		return
	}
	h.respond(w, r, http.StatusCreated, ws, id)
}

// audJSONEqual compares two stored JSON texts as Python values (key order and number spelling aside).
func audJSONEqual(a, b []byte) bool {
	return reflect.DeepEqual(audDecodeAny(a), audDecodeAny(b))
}

func (h audCrud) patch(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, err := decodeAudPatch(raw)
	if err != nil {
		h.invalid(w, err)
		return
	}
	// A null on a NOT NULL column is a 422 body error, as the destination PATCH answers (Python let it reach the
	// database and answered the duplicate-name 409; F15).
	var nulls []httpx.FieldError
	if in.Name.Null {
		nulls = append(nulls, destNullField("name", "string_type", "Input should be a valid string"))
	}
	if in.Description.Null {
		nulls = append(nulls, destNullField("description", "string_type", "Input should be a valid string"))
	}
	if in.RefreshEnabled.Null {
		nulls = append(nulls, destNullField("refresh_enabled", "bool_type", "Input should be a valid boolean"))
	}
	if in.Interval.Null {
		nulls = append(nulls, destNullField("refresh_interval_minutes", "int_type", "Input should be a valid integer"))
	}
	if len(nulls) > 0 {
		httpx.ValidationError(w, nulls...)
		return
	}
	id, ok := h.audienceID(w, r)
	if !ok {
		return
	}
	ws := h.ws(r).WorkspaceID
	var cur gen.Audience
	err = tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		a, err := q.AudCrudGetForUpdate(r.Context(), gen.AudCrudGetForUpdateParams{ID: id, WorkspaceID: ws})
		if err != nil {
			return err
		}
		next := a
		if in.Name.Set {
			next.Name = in.Name.Val
		}
		if in.Description.Set {
			next.Description = in.Description.Val
		}
		if in.Filters.Set {
			next.Filters = []byte(in.Filters.Val)
			if in.Filters.Null {
				next.Filters = []byte("null")
			}
		}
		if in.RefreshEnabled.Set {
			next.RefreshEnabled = in.RefreshEnabled.Val
		}
		if in.Interval.Set {
			next.RefreshIntervalMinutes = int32(in.Interval.Val)
		}
		// SQLAlchemy flushes (and bumps updated_at) only when an attribute actually changed.
		if next.Name != a.Name || next.Description != a.Description || next.RefreshEnabled != a.RefreshEnabled ||
			next.RefreshIntervalMinutes != a.RefreshIntervalMinutes || !audJSONEqual(next.Filters, a.Filters) {
			if err := q.AudCrudUpdate(r.Context(), gen.AudCrudUpdateParams{ID: id, WorkspaceID: ws, Name: next.Name, Description: next.Description,
				Filters: next.Filters, RefreshEnabled: next.RefreshEnabled, RefreshIntervalMinutes: next.RefreshIntervalMinutes}); err != nil {
				return err
			}
		}
		cur = next
		return nil
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Detail(w, http.StatusNotFound, audNotFound)
		return
	case audIsUnique(err):
		httpx.Detail(w, http.StatusConflict, audDuplicateName)
		return
	case err != nil:
		h.internal(w, "patch", err)
		return
	}
	if in.Filters.Set {
		if err := h.materialize(r, ws, id); err != nil {
			h.refreshFailed(w, err)
			return
		}
	}
	if in.RefreshEnabled.Set || in.Interval.Set {
		if err := h.reschedule(r, ws, audiencesched.ScheduleInput{ID: id, WorkspaceID: ws, RefreshEnabled: cur.RefreshEnabled,
			IntervalMinutes: int(cur.RefreshIntervalMinutes)}); err != nil {
			h.internal(w, "schedule", err)
			return
		}
	}
	h.respond(w, r, http.StatusOK, ws, id)
}

func (h audCrud) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := h.audienceID(w, r)
	if !ok {
		return
	}
	ws := h.ws(r).WorkspaceID
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.AudCrudGetForUpdate(r.Context(), gen.AudCrudGetForUpdateParams{ID: id, WorkspaceID: ws}); err != nil {
			return err
		}
		if err := audiencesched.RemoveSchedule(r.Context(), tx, id); err != nil {
			return err
		}
		_, err := q.AudCrudDelete(r.Context(), gen.AudCrudDeleteParams{ID: id, WorkspaceID: ws})
		return err // members, events and destinations go with the ON DELETE CASCADE foreign keys
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Detail(w, http.StatusNotFound, audNotFound)
		return
	}
	if err != nil {
		h.internal(w, "delete", err)
		return
	}
	w.Header().Set("Content-Type", "application/json") // FastAPI keeps it on a 204
	w.WriteHeader(http.StatusNoContent)
}
