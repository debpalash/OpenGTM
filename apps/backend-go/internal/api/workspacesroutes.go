package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// RegisterWorkspaces registers /api/workspaces (any authenticated active user;
// no X-Workspace-Id, the caller's own memberships define what is visible).
func RegisterWorkspaces(r *httpx.Router, d Deps) {
	w := wsRoutes{d}
	mw := identity.Authenticate(d.Signer, d.Identity)
	r.Handle("GET /api/workspaces", http.HandlerFunc(w.list), mw)
	r.Handle("POST /api/workspaces", http.HandlerFunc(w.create), mw)
	// Literal segments win over {ws_id} by ServeMux specificity.
	r.Handle("POST /api/workspaces/switch", http.HandlerFunc(w.switchTo), mw)
	r.Handle("GET /api/workspaces/dashboard", http.HandlerFunc(w.dashboard), mw)
	r.Handle("GET /api/workspaces/{ws_id}", http.HandlerFunc(w.get), mw)
	r.Handle("DELETE /api/workspaces/{ws_id}", http.HandlerFunc(w.remove), mw)
}

type wsRoutes struct{ d Deps }

func (w wsRoutes) fail(rw http.ResponseWriter, what string) {
	l := w.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("workspaces: " + what + " failed")
	httpx.Detail(rw, http.StatusInternalServerError, "Internal Server Error")
}

func (w wsRoutes) q() *gen.Queries { return gen.New(w.d.Pool) }

func userID(r *http.Request) int32 {
	p, _ := identity.PrincipalFrom(r.Context())
	return p.User.ID
}

// epochFloat marshals like Python's float repr: always a fractional part
// (1788000000.0), shortest round-trip digits.
type epochFloat float64

func (f epochFloat) MarshalJSON() ([]byte, error) {
	s := strconv.FormatFloat(float64(f), 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return []byte(s), nil
}

func nowEpoch() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// wsView is the shared workspace shape; omitted fields are dropped per route.
type wsItem struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Description string      `json:"description"`
	Icon        string      `json:"icon"`
	OwnerID     *int32      `json:"owner_id"`
	LeadsCount  int64       `json:"leads_count"`
	IsActive    *bool       `json:"is_active,omitempty"`
	CreatedAt   *epochFloat `json:"created_at"`
}

func toItem(r gen.Workspace, leads int64) wsItem {
	it := wsItem{ID: r.ID, Name: r.Name, Slug: r.Slug, Description: r.Description, Icon: r.Icon, LeadsCount: leads}
	if r.OwnerID.Valid {
		v := r.OwnerID.Int32
		it.OwnerID = &v
	}
	if r.CreatedAt.Valid {
		v := epochFloat(r.CreatedAt.Float64)
		it.CreatedAt = &v
	}
	return it
}

// leads counts leads for one workspace under its RLS scope.
func (w wsRoutes) leads(ctx context.Context, id string) (int64, error) {
	var n int64
	err := tenancy.InWorkspace(ctx, w.d.Pool, id, func(tx pgx.Tx) error {
		var err error
		n, err = gen.New(tx).CountLeads(ctx)
		return err
	})
	return n, err
}

// userWorkspaces lists the caller's workspaces with leads counts.
func (w wsRoutes) userWorkspaces(ctx context.Context, uid int32) ([]gen.Workspace, []int64, error) {
	rows, err := w.q().ListWorkspacesForUser(ctx, uid)
	if err != nil {
		return nil, nil, err
	}
	counts := make([]int64, len(rows))
	for i, row := range rows {
		if counts[i], err = w.leads(ctx, row.ID); err != nil {
			return nil, nil, err
		}
	}
	return rows, counts, nil
}

// activeID mirrors get_user_active_workspace: the stored one while still a
// member, else the lowest accessible id, else null.
func (w wsRoutes) activeID(ctx context.Context, uid int32) (*string, error) {
	q := w.q()
	cur, err := q.ActiveWorkspace(ctx, uid)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		ok, err := q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: cur, UserID: uid})
		if err != nil {
			return nil, err
		}
		if ok {
			return &cur, nil
		}
	}
	first, err := q.FirstAccessibleWorkspace(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &first, nil
}

func (w wsRoutes) list(rw http.ResponseWriter, r *http.Request) {
	ctx, uid := r.Context(), userID(r)
	active, err := w.activeID(ctx, uid)
	if err != nil {
		w.fail(rw, "active")
		return
	}
	rows, counts, err := w.userWorkspaces(ctx, uid)
	if err != nil {
		w.fail(rw, "list")
		return
	}
	items := make([]wsItem, len(rows))
	for i, row := range rows {
		items[i] = toItem(row, counts[i])
		isActive := active != nil && *active == row.ID
		items[i].IsActive = &isActive
	}
	httpx.WriteJSON(rw, http.StatusOK, map[string]any{"workspaces": items, "active_id": active})
}

// slugify is pg_store.slugify: lower, spaces/underscores to '-', keep alnum and '-'.
func slugify(name string) string {
	s := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(name), " ", "-"), "_", "-")
	var b strings.Builder
	for _, c := range s {
		if c == '-' || unicode.IsLetter(c) || unicode.IsDigit(c) || unicode.IsNumber(c) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func uuid4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// bodyStrings reads a JSON object with required and optional string fields,
// answering FastAPI's 422 on any problem.
func bodyStrings(rw http.ResponseWriter, r *http.Request, required []string, optional map[string]string) (map[string]string, bool) {
	var errs []httpx.FieldError
	out := map[string]string{}
	for k, v := range optional {
		out[k] = v
	}
	raw, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxUsersBody))
	var body any
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	if err != nil {
		errs = append(errs, httpx.FieldError{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}})
	} else if obj, isObj := body.(map[string]any); !isObj {
		errs = append(errs, httpx.FieldError{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body})
	} else {
		check := func(n string, mandatory bool) {
			v, present := obj[n]
			s, isStr := v.(string)
			switch {
			case !present && mandatory:
				errs = append(errs, httpx.FieldError{Type: "missing", Loc: []any{"body", n}, Msg: "Field required", Input: obj})
			case !present:
			case !isStr:
				errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", n}, Msg: "Input should be a valid string", Input: v})
			default:
				out[n] = s
			}
		}
		for _, n := range required {
			check(n, true)
		}
		for n := range optional {
			check(n, false)
		}
	}
	if len(errs) > 0 {
		httpx.ValidationError(rw, errs...)
		return nil, false
	}
	return out, true
}

func isSlugCollision(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "workspaces_slug_key"
}

func (w wsRoutes) create(rw http.ResponseWriter, r *http.Request) {
	f, ok := bodyStrings(rw, r, []string{"name"}, map[string]string{"description": "", "icon": "🏢"})
	if !ok {
		return
	}
	ctx, uid := r.Context(), userID(r)
	id, err := uuid4()
	if err != nil {
		w.fail(rw, "id")
		return
	}
	slug, now := slugify(f["name"]), nowEpoch()
	// Workspace, owner membership and the active pointer commit together.
	tx, err := w.d.Pool.Begin(ctx)
	if err != nil {
		w.fail(rw, "begin")
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := gen.New(tx)
	if _, err = q.InsertWorkspace(ctx, gen.InsertWorkspaceParams{ID: id, Name: f["name"], Slug: slug,
		Description: f["description"], Icon: f["icon"], OwnerID: uid, Now: now}); err != nil {
		if isSlugCollision(err) {
			httpx.Detail(rw, http.StatusConflict, "Workspace slug already exists")
			return
		}
		w.fail(rw, "insert")
		return
	}
	if err = q.UpsertOwnerMember(ctx, gen.UpsertOwnerMemberParams{WorkspaceID: id, UserID: uid, Now: now}); err == nil {
		err = q.SetActiveWorkspace(ctx, gen.SetActiveWorkspaceParams{UserID: uid, WorkspaceID: id})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		w.fail(rw, "create")
		return
	}
	httpx.WriteJSON(rw, http.StatusOK, map[string]string{"id": id, "name": f["name"], "slug": slug})
}

func (w wsRoutes) switchTo(rw http.ResponseWriter, r *http.Request) {
	f, ok := bodyStrings(rw, r, []string{"workspace_id"}, nil)
	if !ok {
		return
	}
	ctx, uid, q := r.Context(), userID(r), w.q()
	member, err := q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: f["workspace_id"], UserID: uid})
	if err != nil {
		w.fail(rw, "member")
		return
	}
	if !member {
		httpx.Detail(rw, http.StatusForbidden, "Workspace access denied")
		return
	}
	if err = q.SetActiveWorkspace(ctx, gen.SetActiveWorkspaceParams{UserID: uid, WorkspaceID: f["workspace_id"]}); err != nil {
		w.fail(rw, "switch")
		return
	}
	ws, err := q.WorkspaceByID(ctx, f["workspace_id"])
	if err != nil {
		w.fail(rw, "lookup")
		return
	}
	httpx.WriteJSON(rw, http.StatusOK, map[string]string{"active": ws.Name, "id": ws.ID})
}

func (w wsRoutes) dashboard(rw http.ResponseWriter, r *http.Request) {
	rows, counts, err := w.userWorkspaces(r.Context(), userID(r))
	if err != nil {
		w.fail(rw, "dashboard")
		return
	}
	type entry struct {
		ID         string      `json:"id"`
		Name       string      `json:"name"`
		Slug       string      `json:"slug"`
		Icon       string      `json:"icon"`
		LeadsCount int64       `json:"leads_count"`
		CreatedAt  *epochFloat `json:"created_at"`
	}
	var total int64
	out := make([]entry, len(rows))
	for i, row := range rows {
		it := toItem(row, counts[i])
		total += counts[i]
		out[i] = entry{it.ID, it.Name, it.Slug, it.Icon, it.LeadsCount, it.CreatedAt}
	}
	httpx.WriteJSON(rw, http.StatusOK, map[string]any{"total_workspaces": len(rows), "total_leads": total, "workspaces": out})
}

func (w wsRoutes) get(rw http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("ws_id")
	q := w.q()
	member, err := q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: id, UserID: userID(r)})
	if err != nil {
		w.fail(rw, "member")
		return
	}
	ws, err := q.WorkspaceByID(ctx, id)
	if !member || errors.Is(err, pgx.ErrNoRows) {
		httpx.Detail(rw, http.StatusNotFound, "Workspace not found")
		return
	}
	if err != nil {
		w.fail(rw, "lookup")
		return
	}
	n, err := w.leads(ctx, id)
	if err != nil {
		w.fail(rw, "leads")
		return
	}
	it := toItem(ws, n) // no is_active key on this route
	httpx.WriteJSON(rw, http.StatusOK, it)
}

func (w wsRoutes) remove(rw http.ResponseWriter, r *http.Request) {
	ctx, uid, id := r.Context(), userID(r), r.PathValue("ws_id")
	q := w.q()
	ws, err := q.WorkspaceByID(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		w.fail(rw, "lookup")
		return
	}
	missing := errors.Is(err, pgx.ErrNoRows)
	member := false
	if !missing {
		if member, err = q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: id, UserID: uid}); err != nil {
			w.fail(rw, "member")
			return
		}
	}
	if missing || !member {
		httpx.Detail(rw, http.StatusNotFound, "Workspace not found")
		return
	}
	if !ws.OwnerID.Valid || ws.OwnerID.Int32 != uid {
		httpx.Detail(rw, http.StatusForbidden, "Only the owner can delete a workspace")
		return
	}
	if ws.Slug == "main" {
		httpx.Detail(rw, http.StatusBadRequest, "Cannot delete default workspace")
		return
	}
	// One transaction scoped to the workspace: RLS children need the GUC, the
	// identity tables simply ignore it. Order is manager.delete_workspace's.
	err = tenancy.InWorkspace(ctx, w.d.Pool, id, func(tx pgx.Tx) error {
		g := gen.New(tx)
		for _, del := range []func(context.Context, string) error{
			g.DeleteWorkspaceSettings, g.DeleteWorkspaceOIDCIdentities, g.DeleteWorkspaceSCIMTokens,
			g.DeleteWorkspaceSCIMUsers, g.DeleteWorkspaceSCIMGroupMembers, g.DeleteWorkspaceSCIMGroups,
			g.DeleteWorkspaceMemberPermissions, g.DeleteWorkspaceMembers, g.DeleteWorkspaceActive,
			g.DeleteWorkspaceRow,
		} {
			if err := del(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		w.fail(rw, "delete")
		return
	}
	httpx.WriteJSON(rw, http.StatusOK, map[string]string{"status": "ok"})
}
