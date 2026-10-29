package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const (
	scimGroupExists    = "SCIM group already exists"
	scimGroupMissing   = "SCIM group not found"
	scimMemberNotInt   = "Each group member requires a numeric value"
	pgUniqueViolation  = "23505"
	scimGroupNameLimit = 150
)

var (
	scimGroupFilter = regexp.MustCompile(`(?i)^[\s\p{Z}]*displayName[\s\p{Z}]+eq[\s\p{Z}]+"([^"]+)"[\s\p{Z}]*$`)
	scimRemovePath  = regexp.MustCompile(`(?i)^members\[value[\s\p{Z}]+eq[\s\p{Z}]+"([0-9]+)"\]$`)
	pyIntString     = regexp.MustCompile(`^[+-]?[0-9]+(_[0-9]+)*$`)
)

func registerSCIMGroups(r *httpx.Router, s scimRoutes, base string) {
	r.Handle("GET "+base+"/Groups", http.HandlerFunc(s.listGroups))
	r.Handle("POST "+base+"/Groups", http.HandlerFunc(s.createGroup))
	r.Handle("GET "+base+"/Groups/{group_id}", http.HandlerFunc(s.getGroup))
	r.Handle("PUT "+base+"/Groups/{group_id}", http.HandlerFunc(s.replaceGroup))
	r.Handle("PATCH "+base+"/Groups/{group_id}", http.HandlerFunc(s.patchGroup))
	r.Handle("DELETE "+base+"/Groups/{group_id}", http.HandlerFunc(s.deleteGroup))
}

// scimGroupInput is ScimGroupInput after validation.
type scimGroupInput struct {
	displayName, externalID string
	members                 []any // each element is a decoded JSON object
}

func parseScimGroupInput(obj map[string]any) (scimGroupInput, []mcpFieldErr) {
	var in scimGroupInput
	var errs []mcpFieldErr
	scimSchemasField(obj, false, &errs)
	if v, present := obj["displayName"]; !present {
		errs = append(errs, mcpFieldErr{Type: "missing", Loc: []any{"body", "displayName"}, Msg: "Field required", Input: obj})
	} else if str, ok := v.(string); !ok {
		errs = append(errs, mcpFieldErr{Type: "string_type", Loc: []any{"body", "displayName"}, Msg: "Input should be a valid string", Input: v})
	} else {
		switch n := utf8.RuneCountInString(str); {
		case n < 1:
			errs = append(errs, mcpFieldErr{Type: "string_too_short", Loc: []any{"body", "displayName"}, Msg: "String should have at least 1 character", Input: str, Ctx: map[string]any{"min_length": 1}})
		case n > scimGroupNameLimit:
			errs = append(errs, mcpFieldErr{Type: "string_too_long", Loc: []any{"body", "displayName"}, Msg: "String should have at most 150 characters", Input: str, Ctx: map[string]any{"max_length": scimGroupNameLimit}})
		default:
			in.displayName = str
		}
	}
	if str, ok := scimStrField(obj, "externalId", &errs); ok {
		in.externalID = str
	}
	if v, present := obj["members"]; present {
		list, ok := v.([]any)
		if !ok {
			errs = append(errs, mcpFieldErr{Type: "list_type", Loc: []any{"body", "members"}, Msg: "Input should be a valid list", Input: v})
		}
		for i, el := range list {
			if _, isObj := el.(map[string]any); !isObj {
				errs = append(errs, mcpFieldErr{Type: "dict_type", Loc: []any{"body", "members", i}, Msg: "Input should be a valid dictionary", Input: el})
				continue
			}
			in.members = append(in.members, el)
		}
	}
	return in, errs
}

// pyIntOf is Python int(x) for a decoded JSON value.
func pyIntOf(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case json.Number:
		if n, ok := new(big.Int).SetString(x.String(), 10); ok {
			return n, true
		}
		f, err := strconv.ParseFloat(x.String(), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, false
		}
		n, _ := new(big.Float).SetFloat64(math.Trunc(f)).Int(nil)
		return n, true
	case string:
		t := strings.TrimSpace(x)
		if !pyIntString.MatchString(t) {
			return nil, false
		}
		n, ok := new(big.Int).SetString(strings.ReplaceAll(t, "_", ""), 10)
		return n, ok
	}
	return nil, false
}

// memberIDs is routers/scim.py:_member_ids over a decoded JSON value: any
// shape other than a list of objects with an int-convertible "value" is a 400.
func memberIDs(members any) ([]*big.Int, error) {
	if !pyTruthy(members) {
		return nil, nil
	}
	list, ok := members.([]any)
	if !ok {
		return nil, scimErr{http.StatusBadRequest, scimMemberNotInt}
	}
	out := make([]*big.Int, 0, len(list))
	for _, el := range list {
		obj, isObj := el.(map[string]any)
		if !isObj {
			return nil, scimErr{http.StatusBadRequest, scimMemberNotInt}
		}
		n, ok := pyIntOf(obj["value"])
		if !ok {
			return nil, scimErr{http.StatusBadRequest, scimMemberNotInt}
		}
		out = append(out, n)
	}
	return out, nil
}

// uniqueIDs is list(dict.fromkeys(ids)).
func uniqueIDs(ids []*big.Int) []*big.Int {
	seen := map[string]bool{}
	out := make([]*big.Int, 0, len(ids))
	for _, n := range ids {
		if k := n.String(); !seen[k] {
			seen[k] = true
			out = append(out, n)
		}
	}
	return out
}

// validatedMembers is _scim_validated_ids: every id needs a SCIM user mapping in this workspace.
func validatedMembers(ctx context.Context, q *gen.Queries, wsID string, ids []*big.Int) ([]int32, error) {
	out := make([]int32, 0, len(ids))
	for _, n := range uniqueIDs(ids) {
		found := false
		if n.IsInt64() && n.Int64() >= math.MinInt32 && n.Int64() <= math.MaxInt32 {
			var err error
			if found, err = q.ScimMappingExists(ctx, gen.ScimMappingExistsParams{WorkspaceID: wsID, UserID: int32(n.Int64())}); err != nil {
				return nil, err
			}
		}
		if !found {
			return nil, scimErr{http.StatusBadRequest, fmt.Sprintf("SCIM user %s not found", n)}
		}
		out = append(out, int32(n.Int64()))
	}
	return out, nil
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == pgUniqueViolation
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func scimGroupResource(slug string, g gen.WorkspaceScimGroup, members []int32) jsonObj {
	refs := make([]jsonObj, len(members))
	for i, id := range members {
		v := strconv.Itoa(int(id))
		refs[i] = jsonObj{"value": v, "$ref": "/scim/v2/" + slug + "/Users/" + v}
	}
	return jsonObj{
		"schemas": []string{scimGroupSchema}, "id": g.ID, "externalId": g.ExternalID, "displayName": g.DisplayName,
		"members": refs,
		"meta": jsonObj{
			"resourceType": "Group", "created": scimISO(g.CreatedAt), "lastModified": scimISO(g.UpdatedAt),
			"location": "/scim/v2/" + slug + "/Groups/" + g.ID,
		},
	}
}

// loadGroup is scim.get_group; scimErr 404 when absent.
func loadGroup(ctx context.Context, q *gen.Queries, wsID, id string) (gen.WorkspaceScimGroup, []int32, error) {
	g, err := q.ScimGetGroup(ctx, gen.ScimGetGroupParams{WorkspaceID: wsID, GroupID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return g, nil, scimErr{http.StatusNotFound, scimGroupMissing}
	}
	if err != nil {
		return g, nil, err
	}
	members, err := q.ScimGroupMemberIDs(ctx, gen.ScimGroupMemberIDsParams{WorkspaceID: wsID, GroupID: id})
	return g, members, err
}

// writeGroupErr maps a transaction error. conflict is the status a unique
// violation on (workspace, displayName) answers.
func (s scimRoutes) writeGroupErr(w http.ResponseWriter, err error, what string) {
	if isUniqueViolation(err) {
		httpx.Detail(w, http.StatusConflict, scimGroupExists)
		return
	}
	s.writeErr(w, err, what)
}

// saveGroup is scim.replace_group inside an open workspace transaction.
func saveGroup(ctx context.Context, q *gen.Queries, wsID, id, displayName, externalID string, ids []*big.Int) error {
	members, err := validatedMembers(ctx, q, wsID, ids)
	if err != nil {
		return err
	}
	now := nowSeconds()
	if err := q.ScimUpdateGroup(ctx, gen.ScimUpdateGroupParams{DisplayName: displayName, ExternalID: externalID, Now: now, WorkspaceID: wsID, GroupID: id}); err != nil {
		return err
	}
	if err := q.ScimDeleteGroupMembers(ctx, gen.ScimDeleteGroupMembersParams{WorkspaceID: wsID, GroupID: id}); err != nil {
		return err
	}
	for _, uid := range members {
		if err := q.ScimInsertGroupMember(ctx, gen.ScimInsertGroupMemberParams{WorkspaceID: wsID, GroupID: id, UserID: uid, Now: now}); err != nil {
			return err
		}
	}
	return nil
}

// ---- reads ----

func (s scimRoutes) listGroups(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var errs []mcpFieldErr
	filter, _ := lastQuery(query, "filter")
	one, zero, maxCount := int64(1), int64(0), int64(200)
	startIndex, count := int64(1), int64(100)
	if raw, ok := lastQuery(query, "startIndex"); ok {
		if v, e := scimIntParam(raw, []any{"query", "startIndex"}, &one, nil); e != nil {
			errs = append(errs, *e)
		} else {
			startIndex = v
		}
	}
	if raw, ok := lastQuery(query, "count"); ok {
		if v, e := scimIntParam(raw, []any{"query", "count"}, &zero, &maxCount); e != nil {
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
	var name pgtype.Text
	if filter != "" {
		match := scimGroupFilter.FindStringSubmatch(filter)
		if match == nil {
			httpx.Detail(w, http.StatusBadRequest, "Only displayName eq filtering is supported")
			return
		}
		name = pgtype.Text{String: match[1], Valid: true}
	}
	ctx := r.Context()
	offset := startIndex - 1
	var total int64
	resources := []jsonObj{}
	err := tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		var e error
		if total, e = q.ScimCountGroups(ctx, gen.ScimCountGroupsParams{WorkspaceID: ws.ID, DisplayName: name}); e != nil {
			return e
		}
		if count == 0 || offset > math.MaxInt32 {
			return nil
		}
		groups, e := q.ScimListGroupsPage(ctx, gen.ScimListGroupsPageParams{WorkspaceID: ws.ID, DisplayName: name, PageLimit: int32(count), PageOffset: int32(offset)})
		if e != nil || len(groups) == 0 {
			return e
		}
		ids := make([]string, len(groups))
		for i, g := range groups {
			ids[i] = g.ID
		}
		rows, e := q.ScimGroupMembersFor(ctx, gen.ScimGroupMembersForParams{WorkspaceID: ws.ID, GroupIds: ids})
		if e != nil {
			return e
		}
		by := map[string][]int32{}
		for _, row := range rows {
			by[row.GroupID] = append(by[row.GroupID], row.UserID)
		}
		for _, g := range groups {
			resources = append(resources, scimGroupResource(ws.Slug, g, by[g.ID]))
		}
		return nil
	})
	if err != nil {
		s.fail(w, "list groups")
		return
	}
	writeSCIM(w, http.StatusOK, jsonObj{
		"schemas": []string{scimListSchema}, "totalResults": total, "startIndex": startIndex,
		"itemsPerPage": len(resources), "Resources": resources,
	})
}

func (s scimRoutes) getGroup(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var res jsonObj
	err := tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		g, members, e := loadGroup(ctx, gen.New(tx), ws.ID, r.PathValue("group_id"))
		if e == nil {
			res = scimGroupResource(ws.Slug, g, members)
		}
		return e
	})
	if err != nil {
		s.writeErr(w, err, "get group")
		return
	}
	writeSCIM(w, http.StatusOK, res)
}

// ---- writes ----

func (s scimRoutes) createGroup(w http.ResponseWriter, r *http.Request) {
	obj, errs, ok := scimDecodeObject(w, r)
	if !ok {
		return
	}
	var in scimGroupInput
	if errs == nil {
		in, errs = parseScimGroupInput(obj)
	}
	if len(errs) > 0 {
		writeFieldErrs(w, errs)
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	// FastAPI's create_group wraps _member_ids in a generic `except Exception`,
	// so a non-numeric member answers 409 here (PUT/PATCH answer 400).
	ids, err := memberIDs(in.members)
	if err != nil {
		httpx.Detail(w, http.StatusConflict, scimGroupExists)
		return
	}
	id, err := newUUID()
	if err != nil {
		s.fail(w, "group id")
		return
	}
	ctx := r.Context()
	var res jsonObj
	err = tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		members, e := validatedMembers(ctx, q, ws.ID, ids)
		if e != nil {
			return e
		}
		now := nowSeconds()
		if e = q.ScimInsertGroup(ctx, gen.ScimInsertGroupParams{ID: id, WorkspaceID: ws.ID, ExternalID: in.externalID, DisplayName: strings.TrimSpace(in.displayName), Now: now}); e != nil {
			return e
		}
		for _, uid := range members {
			if e = q.ScimInsertGroupMember(ctx, gen.ScimInsertGroupMemberParams{WorkspaceID: ws.ID, GroupID: id, UserID: uid, Now: now}); e != nil {
				return e
			}
		}
		g, ms, e := loadGroup(ctx, q, ws.ID, id)
		if e == nil {
			res = scimGroupResource(ws.Slug, g, ms)
		}
		return e
	})
	if err != nil {
		s.writeGroupErr(w, err, "create group")
		return
	}
	writeSCIM(w, http.StatusCreated, res)
}

func (s scimRoutes) replaceGroup(w http.ResponseWriter, r *http.Request) {
	obj, errs, ok := scimDecodeObject(w, r)
	if !ok {
		return
	}
	var in scimGroupInput
	if errs == nil {
		in, errs = parseScimGroupInput(obj)
	}
	if len(errs) > 0 {
		writeFieldErrs(w, errs)
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	s.mutateGroup(w, r, ws, "replace group", func(g gen.WorkspaceScimGroup, _ []int32) (string, string, []*big.Int, error) {
		ids, err := memberIDs(in.members)
		return strings.TrimSpace(in.displayName), in.externalID, ids, err
	})
}

func (s scimRoutes) patchGroup(w http.ResponseWriter, r *http.Request) {
	obj, errs, ok := scimDecodeObject(w, r)
	if !ok {
		return
	}
	var schemas []string
	var ops []map[string]any
	if errs == nil {
		schemas, ops, errs = parsePatchInput(obj)
	}
	if len(errs) > 0 {
		writeFieldErrs(w, errs)
		return
	}
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	s.mutateGroup(w, r, ws, "patch group", func(g gen.WorkspaceScimGroup, current []int32) (string, string, []*big.Int, error) {
		hasSchema := false
		for _, sc := range schemas {
			hasSchema = hasSchema || sc == scimPatchSchema
		}
		if !hasSchema {
			return "", "", nil, scimErr{http.StatusBadRequest, "PatchOp schema is required"}
		}
		displayName, externalID := g.DisplayName, g.ExternalID
		ids := make([]*big.Int, len(current))
		for i, id := range current {
			ids[i] = big.NewInt(int64(id))
		}
		for _, op := range ops {
			name := ""
			if v, present := op["op"]; present {
				name = pyStr(v)
			}
			name = strings.ToLower(name)
			path, value := op["path"], op["value"]
			pathStr, pathIsStr := path.(string)
			switch {
			case (name == "add" || name == "replace") && pathIsStr && pathStr == "members":
				requested, err := memberIDs(value)
				if err != nil {
					return "", "", nil, err
				}
				if name == "replace" {
					ids = requested
				} else {
					ids = uniqueIDs(append(ids, requested...))
				}
			case name == "remove" && pathIsStr:
				m := scimRemovePath.FindStringSubmatch(pathStr)
				if m == nil {
					return "", "", nil, scimErr{http.StatusBadRequest, "Unsupported group remove path"}
				}
				drop, _ := new(big.Int).SetString(m[1], 10)
				kept := ids[:0:0]
				for _, id := range ids {
					if id.Cmp(drop) != 0 {
						kept = append(kept, id)
					}
				}
				ids = kept
			case (name == "add" || name == "replace") && pathIsStr && pathStr == "displayName":
				displayName = pyStr(value)
			case (name == "add" || name == "replace") && pathIsStr && pathStr == "externalId":
				externalID = pyStr(value)
			default:
				return "", "", nil, scimErr{http.StatusBadRequest, "Unsupported group patch operation: " + name + " " + pyStr(path)}
			}
		}
		return displayName, externalID, ids, nil
	})
}

// mutateGroup loads the group (404), asks derive for the new state, saves it
// atomically and writes the resource.
func (s scimRoutes) mutateGroup(w http.ResponseWriter, r *http.Request, ws gen.Workspace, what string,
	derive func(gen.WorkspaceScimGroup, []int32) (string, string, []*big.Int, error)) {
	ctx := r.Context()
	id := r.PathValue("group_id")
	var res jsonObj
	err := tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		g, members, e := loadGroup(ctx, q, ws.ID, id)
		if e != nil {
			return e
		}
		name, ext, ids, e := derive(g, members)
		if e != nil {
			return e
		}
		if e = saveGroup(ctx, q, ws.ID, id, name, ext, ids); e != nil {
			return e
		}
		g, members, e = loadGroup(ctx, q, ws.ID, id)
		if e == nil {
			res = scimGroupResource(ws.Slug, g, members)
		}
		return e
	})
	if err != nil {
		s.writeGroupErr(w, err, what)
		return
	}
	writeSCIM(w, http.StatusOK, res)
}

func (s scimRoutes) deleteGroup(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("group_id")
	err := tenancy.InWorkspace(ctx, s.d.Pool, ws.ID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if e := q.ScimDeleteGroupMembers(ctx, gen.ScimDeleteGroupMembersParams{WorkspaceID: ws.ID, GroupID: id}); e != nil {
			return e
		}
		n, e := q.ScimDeleteGroup(ctx, gen.ScimDeleteGroupParams{WorkspaceID: ws.ID, GroupID: id})
		if e == nil && n == 0 {
			return scimErr{http.StatusNotFound, scimGroupMissing}
		}
		return e
	})
	if err != nil {
		s.writeErr(w, err, "delete group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
