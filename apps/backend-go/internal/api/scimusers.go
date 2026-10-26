package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// scimUserNameFilter is routers/scim.py's re.fullmatch(..., IGNORECASE).
var scimUserNameFilter = regexp.MustCompile(`(?i)^[\s\p{Z}]*userName[\s\p{Z}]+eq[\s\p{Z}]+"([^"]+)"[\s\p{Z}]*$`)

// scimErr is an HTTPException raised inside a transaction.
type scimErr struct {
	status int
	detail string
}

func (e scimErr) Error() string { return e.detail }

func (s scimRoutes) writeErr(w http.ResponseWriter, err error, what string) {
	var se scimErr
	if errors.As(err, &se) {
		httpx.Detail(w, se.status, se.detail)
		return
	}
	s.fail(w, what)
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// scimUserInput is ScimUserInput after validation.
type scimUserInput struct {
	userName, externalID, displayName string
	active                            bool
}

func parseScimUserInput(obj map[string]any) (scimUserInput, []mcpFieldErr) {
	in := scimUserInput{active: true}
	var errs []mcpFieldErr
	scimSchemasField(obj, false, &errs)
	if v, present := obj["userName"]; !present {
		errs = append(errs, mcpFieldErr{Type: "missing", Loc: []any{"body", "userName"}, Msg: "Field required", Input: obj})
	} else if str, ok := v.(string); !ok {
		errs = append(errs, mcpFieldErr{Type: "string_type", Loc: []any{"body", "userName"}, Msg: "Input should be a valid string", Input: v})
	} else {
		switch n := utf8.RuneCountInString(str); {
		case n < 1:
			errs = append(errs, mcpFieldErr{Type: "string_too_short", Loc: []any{"body", "userName"}, Msg: "String should have at least 1 character", Input: str, Ctx: map[string]any{"min_length": 1}})
		case n > 150:
			errs = append(errs, mcpFieldErr{Type: "string_too_long", Loc: []any{"body", "userName"}, Msg: "String should have at most 150 characters", Input: str, Ctx: map[string]any{"max_length": 150}})
		default:
			in.userName = str
		}
	}
	if str, ok := scimStrField(obj, "externalId", &errs); ok {
		in.externalID = str
	}
	if str, ok := scimStrField(obj, "displayName", &errs); ok {
		in.displayName = str
	}
	if v, present := obj["active"]; present {
		b, e := pyBool(v)
		if e != nil {
			errs = append(errs, *e)
		} else {
			in.active = b
		}
	}
	return in, errs
}

// parsePatchInput validates PatchInput and returns its schemas and operations.
func parsePatchInput(obj map[string]any) ([]string, []map[string]any, []mcpFieldErr) {
	var errs []mcpFieldErr
	schemas := scimSchemasField(obj, true, &errs)
	var ops []map[string]any
	if v, present := obj["Operations"]; !present {
		errs = append(errs, mcpFieldErr{Type: "missing", Loc: []any{"body", "Operations"}, Msg: "Field required", Input: obj})
	} else if list, ok := v.([]any); !ok {
		errs = append(errs, mcpFieldErr{Type: "list_type", Loc: []any{"body", "Operations"}, Msg: "Input should be a valid list", Input: v})
	} else {
		for i, el := range list {
			m, ok := el.(map[string]any)
			if !ok {
				errs = append(errs, mcpFieldErr{Type: "dict_type", Loc: []any{"body", "Operations", i}, Msg: "Input should be a valid dictionary", Input: el})
				continue
			}
			ops = append(ops, m)
		}
	}
	return schemas, ops, errs
}

// userID parses the {user_id} path parameter; ok=false means a 422 was written.
// found=false means the id is outside int32 (no such user).
func userIDParam(raw string) (id int32, found bool, e *mcpFieldErr) {
	n, e := pyInt(raw, []any{"path", "user_id"})
	if e != nil {
		return 0, false, e
	}
	if n.huge || n.v < math.MinInt32 || n.v > math.MaxInt32 {
		return 0, false, nil
	}
	return int32(n.v), true, nil
}

// ---- reads ----

func (s scimRoutes) listUsers(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var errs []mcpFieldErr
	filter, _ := lastQuery(query, "filter")
	one, zero, maxCount := int64(1), int64(0), int64(200)
	startIndex, count := int64(1), int64(100)
	if raw, ok := lastQuery(query, "startIndex"); ok {
		v, e := scimIntParam(raw, []any{"query", "startIndex"}, &one, nil)
		if e != nil {
			errs = append(errs, *e)
		} else {
			startIndex = v
		}
	}
	if raw, ok := lastQuery(query, "count"); ok {
		v, e := scimIntParam(raw, []any{"query", "count"}, &zero, &maxCount)
		if e != nil {
			errs = append(errs, *e)
		} else {
			count = v
		}
	}
	if len(errs) > 0 {
		writeFieldErrs(w, errs)
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	var match []string
	if filter != "" {
		if match = scimUserNameFilter.FindStringSubmatch(filter); match == nil {
			httpx.Detail(w, http.StatusBadRequest, "Only userName eq filtering is supported")
			return
		}
	}
	ctx := r.Context()
	offset := startIndex - 1
	var mappings []gen.WorkspaceScimUser
	var total int64
	err := tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if match != nil {
			u, e := q.ScimUserByLowerUsername(ctx, strings.ToLower(strings.TrimSpace(match[1])))
			if errors.Is(e, pgx.ErrNoRows) {
				return nil
			}
			if e != nil {
				return e
			}
			m, e := q.ScimGetMapping(ctx, gen.ScimGetMappingParams{WorkspaceID: ws.ID, UserID: u.ID})
			if errors.Is(e, pgx.ErrNoRows) {
				return nil
			}
			if e != nil {
				return e
			}
			total = 1
			if offset == 0 && count > 0 {
				mappings = []gen.WorkspaceScimUser{m}
			}
			return nil
		}
		var e error
		if total, e = q.ScimCountMappings(ctx, ws.ID); e != nil {
			return e
		}
		if count > 0 && offset <= math.MaxInt32 {
			mappings, e = q.ScimListMappingsPage(ctx, gen.ScimListMappingsPageParams{WorkspaceID: ws.ID, PageLimit: int32(count), PageOffset: int32(offset)})
		}
		return e
	})
	if err != nil {
		s.fail(w, "list users")
		return
	}
	resources := []jsonObj{}
	if len(mappings) > 0 {
		ids := make([]int32, len(mappings))
		for i, m := range mappings {
			ids[i] = m.UserID
		}
		users, err := gen.New(s.d.Pool).ScimUsersByIDs(ctx, ids)
		if err != nil {
			s.fail(w, "list users")
			return
		}
		byID := make(map[int32]gen.User, len(users))
		for _, u := range users {
			byID[u.ID] = u
		}
		for _, m := range mappings {
			if u, ok := byID[m.UserID]; ok {
				resources = append(resources, scimUserResource(ws.Slug, u.ID, u.Username.String, m))
			}
		}
	}
	writeSCIM(w, http.StatusOK, jsonObj{
		"schemas": []string{scimListSchema}, "totalResults": total, "startIndex": startIndex,
		"itemsPerPage": len(resources), "Resources": resources,
	})
}

// loadMapped returns the mapping and user for id inside a workspace; nil mapping means 404.
func (s scimRoutes) loadMapped(ctx context.Context, ws gen.Workspace, id int32) (*gen.WorkspaceScimUser, identity.User, error) {
	var m gen.WorkspaceScimUser
	err := tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		var e error
		m, e = gen.New(tx).ScimGetMapping(ctx, gen.ScimGetMappingParams{WorkspaceID: ws.ID, UserID: id})
		return e
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, identity.User{}, nil
	}
	if err != nil {
		return nil, identity.User{}, err
	}
	u, err := s.d.Identity.ByID(ctx, id)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, identity.User{}, nil
	}
	if err != nil {
		return nil, identity.User{}, err
	}
	return &m, u, nil
}

func (s scimRoutes) getUser(w http.ResponseWriter, r *http.Request) {
	id, found, e := userIDParam(r.PathValue("user_id"))
	if e != nil {
		writeFieldErrs(w, []mcpFieldErr{*e})
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	if !found {
		httpx.Detail(w, http.StatusNotFound, "SCIM user not found")
		return
	}
	m, u, err := s.loadMapped(r.Context(), ws, id)
	if err != nil {
		s.fail(w, "get user")
		return
	}
	if m == nil {
		httpx.Detail(w, http.StatusNotFound, "SCIM user not found")
		return
	}
	writeSCIM(w, http.StatusOK, scimUserResource(ws.Slug, u.ID, u.Username, *m))
}

// ---- writes ----

func randomPassword() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// upsertMapping is scim.upsert_mapping inside a workspace transaction.
func upsertMapping(ctx context.Context, q *gen.Queries, wsID string, userID int32, externalID, displayName string, active bool) (gen.WorkspaceScimUser, error) {
	if externalID != "" {
		collides, err := q.ScimExternalIDCollision(ctx, gen.ScimExternalIDCollisionParams{WorkspaceID: wsID, ExternalID: externalID, UserID: userID})
		if err != nil {
			return gen.WorkspaceScimUser{}, err
		}
		if collides {
			return gen.WorkspaceScimUser{}, scimErr{http.StatusConflict, "SCIM externalId already exists"}
		}
	}
	return q.ScimUpsertMapping(ctx, gen.ScimUpsertMappingParams{
		WorkspaceID: wsID, UserID: userID, ExternalID: externalID, DisplayName: displayName, Active: active, Now: nowSeconds(),
	})
}

func (s scimRoutes) createUser(w http.ResponseWriter, r *http.Request) {
	obj, errs, ok := scimDecodeObject(w, r)
	if !ok {
		return
	}
	var in scimUserInput
	if errs == nil {
		in, errs = parseScimUserInput(obj)
	}
	if len(errs) > 0 {
		writeFieldErrs(w, errs)
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	username := strings.ToLower(strings.TrimSpace(in.userName))
	user, err := s.d.Identity.ByUsername(ctx, username)
	if errors.Is(err, identity.ErrNotFound) {
		pw, perr := randomPassword()
		var hash string
		if perr == nil {
			hash, perr = auth.HashPassword(pw)
		}
		if perr != nil {
			s.fail(w, "password")
			return
		}
		user, err = s.d.Identity.Insert(ctx, identity.NewUser{Username: username, HashedPassword: hash, IsActive: true, Role: "user"})
	}
	if err != nil {
		s.fail(w, "user lookup")
		return
	}
	var mapping gen.WorkspaceScimUser
	err = tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if _, e := q.ScimGetMapping(ctx, gen.ScimGetMappingParams{WorkspaceID: ws.ID, UserID: user.ID}); e == nil {
			return scimErr{http.StatusConflict, "SCIM userName already exists"}
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if e := upsertCheck(ctx, q, ws.ID, user.ID, in.externalID); e != nil {
			return e
		}
		if in.active {
			if e := ensureViewer(ctx, q, ws.ID, user.ID); e != nil {
				return e
			}
		}
		var e error
		mapping, e = upsertMapping(ctx, q, ws.ID, user.ID, in.externalID, in.displayName, in.active)
		return e
	})
	if err != nil {
		s.writeErr(w, err, "create user")
		return
	}
	writeSCIM(w, http.StatusCreated, scimUserResource(ws.Slug, user.ID, user.Username, mapping))
}

// upsertCheck rejects an externalId already used by another user before
// membership changes, so a 409 leaves no partial membership behind.
func upsertCheck(ctx context.Context, q *gen.Queries, wsID string, userID int32, externalID string) error {
	if externalID == "" {
		return nil
	}
	collides, err := q.ScimExternalIDCollision(ctx, gen.ScimExternalIDCollisionParams{WorkspaceID: wsID, ExternalID: externalID, UserID: userID})
	if err != nil {
		return err
	}
	if collides {
		return scimErr{http.StatusConflict, "SCIM externalId already exists"}
	}
	return nil
}

func ensureViewer(ctx context.Context, q *gen.Queries, wsID string, userID int32) error {
	member, err := q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: wsID, UserID: userID})
	if err != nil || member {
		return err
	}
	return q.ScimAddViewerMember(ctx, gen.ScimAddViewerMemberParams{WorkspaceID: wsID, UserID: userID, Now: nowSeconds()})
}

func removeMember(ctx context.Context, q *gen.Queries, wsID string, userID int32) error {
	member, err := q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: wsID, UserID: userID})
	if err != nil || !member {
		return err
	}
	p := gen.ScimRemoveMemberOIDCParams{WorkspaceID: wsID, UserID: userID}
	if err := q.ScimRemoveMemberOIDC(ctx, p); err != nil {
		return err
	}
	if err := q.ScimRemoveMemberPermissions(ctx, gen.ScimRemoveMemberPermissionsParams(p)); err != nil {
		return err
	}
	return q.ScimRemoveMember(ctx, gen.ScimRemoveMemberParams(p))
}

// casefold approximates str.casefold for the userName immutability check.
func casefold(s string) string { return strings.ToLower(strings.ReplaceAll(s, "ß", "ss")) }

// apply is routers/scim.py:_apply. values holds only the keys the caller set.
func (s scimRoutes) apply(ctx context.Context, ws gen.Workspace, user identity.User, m gen.WorkspaceScimUser, values map[string]any) (gen.WorkspaceScimUser, error) {
	if v, ok := values["userName"]; ok && casefold(strings.TrimSpace(pyStr(v))) != casefold(user.Username) {
		return gen.WorkspaceScimUser{}, scimErr{http.StatusConflict, "userName cannot be changed after provisioning"}
	}
	active := m.Active
	if v, ok := values["active"]; ok {
		active = pyTruthy(v)
	}
	externalID, displayName := m.ExternalID, m.DisplayName
	if v, ok := values["externalId"]; ok {
		externalID = pyStr(v)
	}
	if v, ok := values["displayName"]; ok {
		displayName = pyStr(v)
	}
	var out gen.WorkspaceScimUser
	err := tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if e := upsertCheck(ctx, q, ws.ID, user.ID, externalID); e != nil {
			return e
		}
		var e error
		if active {
			e = ensureViewer(ctx, q, ws.ID, user.ID)
		} else {
			e = removeMember(ctx, q, ws.ID, user.ID)
		}
		if e != nil {
			return e
		}
		out, e = upsertMapping(ctx, q, ws.ID, user.ID, externalID, displayName, active)
		return e
	})
	return out, err
}

func (s scimRoutes) replaceUser(w http.ResponseWriter, r *http.Request) {
	id, found, e := userIDParam(r.PathValue("user_id"))
	var errs []mcpFieldErr
	if e != nil {
		errs = append(errs, *e)
	}
	obj, bodyErrs, ok := scimDecodeObject(w, r)
	if !ok {
		return
	}
	var in scimUserInput
	if bodyErrs == nil {
		in, bodyErrs = parseScimUserInput(obj)
	}
	if errs = append(errs, bodyErrs...); len(errs) > 0 {
		writeFieldErrs(w, errs)
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	s.mutate(w, r, ws, id, found, func(m gen.WorkspaceScimUser, u identity.User) (map[string]any, error) {
		return map[string]any{"userName": in.userName, "externalId": in.externalID, "displayName": in.displayName, "active": in.active}, nil
	})
}

var scimPatchKeys = map[string]bool{"active": true, "displayName": true, "externalId": true, "userName": true}

func (s scimRoutes) patchUser(w http.ResponseWriter, r *http.Request) {
	id, found, e := userIDParam(r.PathValue("user_id"))
	var errs []mcpFieldErr
	if e != nil {
		errs = append(errs, *e)
	}
	obj, bodyErrs, ok := scimDecodeObject(w, r)
	if !ok {
		return
	}
	var schemas []string
	var ops []map[string]any
	if bodyErrs == nil {
		schemas, ops, bodyErrs = parsePatchInput(obj)
	}
	if errs = append(errs, bodyErrs...); len(errs) > 0 {
		writeFieldErrs(w, errs)
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	s.mutate(w, r, ws, id, found, func(m gen.WorkspaceScimUser, u identity.User) (map[string]any, error) {
		hasSchema := false
		for _, sc := range schemas {
			hasSchema = hasSchema || sc == scimPatchSchema
		}
		if !hasSchema {
			return nil, scimErr{http.StatusBadRequest, "PatchOp schema is required"}
		}
		values := map[string]any{}
		for _, op := range ops {
			name := ""
			if v, present := op["op"]; present {
				name = pyStr(v)
			}
			if l := strings.ToLower(name); l != "add" && l != "replace" {
				return nil, scimErr{http.StatusBadRequest, "Only add and replace patch operations are supported"}
			}
			path, value := op["path"], op["value"]
			if p, isStr := path.(string); isStr && scimPatchKeys[p] {
				values[p] = value
			} else if obj, isObj := value.(map[string]any); !pyTruthy(path) && isObj {
				for k, v := range obj {
					if scimPatchKeys[k] {
						values[k] = v
					}
				}
			} else {
				return nil, scimErr{http.StatusBadRequest, "Unsupported patch path: " + pyStr(path)}
			}
		}
		return values, nil
	})
}

// mutate loads the mapped user (404 otherwise), derives the values to apply,
// applies them and writes the resource.
func (s scimRoutes) mutate(w http.ResponseWriter, r *http.Request, ws gen.Workspace, id int32, found bool, values func(gen.WorkspaceScimUser, identity.User) (map[string]any, error)) {
	ctx := r.Context()
	var m *gen.WorkspaceScimUser
	var u identity.User
	if found {
		var err error
		if m, u, err = s.loadMapped(ctx, ws, id); err != nil {
			s.fail(w, "load user")
			return
		}
	}
	if m == nil {
		httpx.Detail(w, http.StatusNotFound, "SCIM user not found")
		return
	}
	vals, err := values(*m, u)
	if err == nil {
		var saved gen.WorkspaceScimUser
		if saved, err = s.apply(ctx, ws, u, *m, vals); err == nil {
			writeSCIM(w, http.StatusOK, scimUserResource(ws.Slug, u.ID, u.Username, saved))
			return
		}
	}
	s.writeErr(w, err, "update user")
}

func (s scimRoutes) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, found, e := userIDParam(r.PathValue("user_id"))
	if e != nil {
		writeFieldErrs(w, []mcpFieldErr{*e})
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if !found {
			return scimErr{http.StatusNotFound, "SCIM user not found"}
		}
		m, e := q.ScimGetMapping(ctx, gen.ScimGetMappingParams{WorkspaceID: ws.ID, UserID: id})
		if errors.Is(e, pgx.ErrNoRows) {
			return scimErr{http.StatusNotFound, "SCIM user not found"}
		}
		if e != nil {
			return e
		}
		if e := removeMember(ctx, q, ws.ID, id); e != nil {
			return e
		}
		// Python re-upserts the mapping with the stored values and active=false.
		_, e = q.ScimUpsertMapping(ctx, gen.ScimUpsertMappingParams{
			WorkspaceID: ws.ID, UserID: id, ExternalID: m.ExternalID, DisplayName: m.DisplayName, Active: false, Now: nowSeconds(),
		})
		return e
	})
	if err != nil {
		s.writeErr(w, err, "delete user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
