package api

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

const (
	scimMaxBody   = 1 << 20
	scimTouchGap  = 60 // seconds between last_used_at updates
	scimDefaultTT = 90 // days, when config carries no value
)

// RegisterSCIM registers /scim/v2/{workspace_slug}. Auth is the workspace SCIM
// bearer token checked inside each handler (no JWT middleware). /scim is not
// exposed through nginx; Go serves it on its own port like Python does.
func RegisterSCIM(r *httpx.Router, d Deps) {
	s := scimRoutes{d}
	const base = "/scim/v2/{workspace_slug}"
	r.Handle("GET "+base+"/ServiceProviderConfig", http.HandlerFunc(s.serviceProviderConfig))
	r.Handle("GET "+base+"/ResourceTypes", http.HandlerFunc(s.resourceTypes))
	r.Handle("GET "+base+"/ResourceTypes/{resource_type}", http.HandlerFunc(s.resourceType))
	r.Handle("GET "+base+"/Schemas", http.HandlerFunc(s.schemas))
	r.Handle("GET "+base+"/Schemas/{schema_id...}", http.HandlerFunc(s.schema))
	r.Handle("GET "+base+"/Users", http.HandlerFunc(s.listUsers))
	r.Handle("POST "+base+"/Users", http.HandlerFunc(s.createUser))
	r.Handle("GET "+base+"/Users/{user_id}", http.HandlerFunc(s.getUser))
	r.Handle("PUT "+base+"/Users/{user_id}", http.HandlerFunc(s.replaceUser))
	r.Handle("PATCH "+base+"/Users/{user_id}", http.HandlerFunc(s.patchUser))
	r.Handle("DELETE "+base+"/Users/{user_id}", http.HandlerFunc(s.deleteUser))
}

type scimRoutes struct{ d Deps }

func (s scimRoutes) fail(w http.ResponseWriter, what string) {
	l := s.d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Error("scim: " + what + " failed")
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// writeSCIM writes a success body as application/scim+json. Error bodies stay
// application/json (FastAPI exception handlers ignore default_response_class).
func writeSCIM(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func (s scimRoutes) ttlSeconds() float64 {
	days := s.d.Config.ScimTokenTTLDays
	if days < 1 {
		days = scimDefaultTT
	}
	return float64(days) * 86400
}

func scimDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// workspace is routers/scim.py:_workspace. It writes the error response and
// returns false when the slug is unknown (404) or the bearer check fails (401).
func (s scimRoutes) workspace(w http.ResponseWriter, r *http.Request) (gen.Workspace, bool) {
	ctx := r.Context()
	q := gen.New(s.d.Pool)
	ws, err := q.WorkspaceBySlug(ctx, r.PathValue("workspace_slug"))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Detail(w, http.StatusNotFound, "Workspace not found")
		return gen.Workspace{}, false
	}
	if err != nil {
		s.fail(w, "workspace lookup")
		return gen.Workspace{}, false
	}
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	ok := false
	if strings.EqualFold(scheme, "bearer") && token != "" {
		row, err := q.ScimTokenRow(ctx, ws.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			s.fail(w, "token lookup")
			return gen.Workspace{}, false
		default:
			now := float64(time.Now().UnixNano()) / 1e9
			exp := row.CreatedAt + s.ttlSeconds()
			if row.ExpiresAt.Valid && row.ExpiresAt.Float64 != 0 {
				exp = row.ExpiresAt.Float64
			}
			ok = exp > now && subtle.ConstantTimeCompare([]byte(row.TokenHash), []byte(scimDigest(token))) == 1
			if ok && (!row.LastUsedAt.Valid || row.LastUsedAt.Float64 == 0 || now-row.LastUsedAt.Float64 >= scimTouchGap) {
				if err := q.ScimTouchToken(ctx, gen.ScimTouchTokenParams{Now: now, WorkspaceID: ws.ID}); err != nil {
					s.fail(w, "token touch")
					return gen.Workspace{}, false
				}
			}
		}
	}
	if !ok {
		httpx.Detail(w, http.StatusUnauthorized, "Invalid SCIM bearer token", httpx.Header{Key: "WWW-Authenticate", Value: "Bearer"})
		return gen.Workspace{}, false
	}
	return ws, true
}

func (s scimRoutes) serviceProviderConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.workspace(w, r); ok {
		writeSCIM(w, http.StatusOK, scimServiceProviderConfig())
	}
}

func scimListResponse(resources any, total, start, per int) jsonObj {
	return jsonObj{"schemas": []string{scimListSchema}, "totalResults": total, "startIndex": start, "itemsPerPage": per, "Resources": resources}
}

func (s scimRoutes) resourceTypes(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	res := make([]jsonObj, 0, len(scimResourceTypeOrder))
	for _, rt := range scimResourceTypeOrder {
		res = append(res, scimResourceType(ws.Slug, rt.kind, rt.schema))
	}
	writeSCIM(w, http.StatusOK, scimListResponse(res, 2, 1, 2))
}

func (s scimRoutes) resourceType(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspace(w, r)
	if !ok {
		return
	}
	name := r.PathValue("resource_type")
	for _, rt := range scimResourceTypeOrder {
		if rt.kind == name {
			writeSCIM(w, http.StatusOK, scimResourceType(ws.Slug, rt.kind, rt.schema))
			return
		}
	}
	httpx.Detail(w, http.StatusNotFound, "Resource type not found")
}

func (s scimRoutes) schemas(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.workspace(w, r); ok {
		writeSCIM(w, http.StatusOK, scimListResponse(scimSchemaResources(), 2, 1, 2))
	}
}

func (s scimRoutes) schema(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.workspace(w, r); !ok {
		return
	}
	id := r.PathValue("schema_id")
	for _, res := range scimSchemaResources() {
		if res["id"] == id {
			writeSCIM(w, http.StatusOK, res)
			return
		}
	}
	httpx.Detail(w, http.StatusNotFound, "Schema not found")
}

// scimISO is datetime.fromtimestamp(x, timezone.utc).isoformat().
func scimISO(x float64) string {
	frac, whole := math.Modf(x)
	us := math.RoundToEven(frac * 1e6)
	sec := int64(whole)
	if us < 0 {
		sec--
		us += 1e6
	}
	if us >= 1e6 {
		sec++
		us -= 1e6
	}
	u := time.Unix(sec, 0).UTC()
	out := u.Format("2006-01-02T15:04:05")
	if us != 0 {
		out += fmt.Sprintf(".%06d", int64(us))
	}
	return out + "+00:00"
}

func scimUserResource(slug string, id int32, username string, m gen.WorkspaceScimUser) jsonObj {
	display := m.DisplayName
	if display == "" {
		display = username
	}
	return jsonObj{
		"schemas": []string{scimUserSchema}, "id": strconv.Itoa(int(id)), "externalId": m.ExternalID,
		"userName": username, "displayName": display, "active": m.Active,
		"meta": jsonObj{
			"resourceType": "User", "created": scimISO(m.CreatedAt), "lastModified": scimISO(m.UpdatedAt),
			"location": "/scim/v2/" + slug + "/Users/" + strconv.Itoa(int(id)),
		},
	}
}

// ---- request validation (pydantic lax-mode equivalents) ----

// scimDecodeObject reads a JSON object body like FastAPI does for a model parameter.
func scimDecodeObject(w http.ResponseWriter, r *http.Request) (map[string]any, []mcpFieldErr, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, scimMaxBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, nil, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, []mcpFieldErr{{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil}}, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body any
	if err := dec.Decode(&body); err != nil || dec.More() {
		return nil, []mcpFieldErr{{Type: "json_invalid", Loc: []any{"body", 0}, Msg: "JSON decode error", Input: map[string]any{}}}, true
	}
	obj, isObj := body.(map[string]any)
	if !isObj {
		return nil, []mcpFieldErr{{Type: "model_attributes_type", Loc: []any{"body"},
			Msg: "Input should be a valid dictionary or object to extract fields from", Input: body}}, true
	}
	return obj, nil, true
}

func scimStrField(obj map[string]any, name string, errs *[]mcpFieldErr) (string, bool) {
	v, present := obj[name]
	if !present {
		return "", false
	}
	str, ok := v.(string)
	if !ok {
		*errs = append(*errs, mcpFieldErr{Type: "string_type", Loc: []any{"body", name}, Msg: "Input should be a valid string", Input: v})
		return "", false
	}
	return str, true
}

func scimSchemasField(obj map[string]any, required bool, errs *[]mcpFieldErr) []string {
	v, present := obj["schemas"]
	if !present {
		if required {
			*errs = append(*errs, mcpFieldErr{Type: "missing", Loc: []any{"body", "schemas"}, Msg: "Field required", Input: obj})
		}
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		*errs = append(*errs, mcpFieldErr{Type: "list_type", Loc: []any{"body", "schemas"}, Msg: "Input should be a valid list", Input: v})
		return nil
	}
	out := make([]string, 0, len(list))
	for i, el := range list {
		str, ok := el.(string)
		if !ok {
			*errs = append(*errs, mcpFieldErr{Type: "string_type", Loc: []any{"body", "schemas", i}, Msg: "Input should be a valid string", Input: el})
			continue
		}
		out = append(out, str)
	}
	return out
}

// pyBool is pydantic's lax bool parsing of a JSON value.
func pyBool(v any) (bool, *mcpFieldErr) {
	loc := []any{"body", "active"}
	switch x := v.(type) {
	case bool:
		return x, nil
	case json.Number:
		switch x.String() {
		case "0", "0.0":
			return false, nil
		case "1", "1.0":
			return true, nil
		}
		return false, &mcpFieldErr{Type: "bool_parsing", Loc: loc, Msg: "Input should be a valid boolean, unable to interpret input", Input: v}
	case string:
		switch strings.ToLower(x) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return false, &mcpFieldErr{Type: "bool_parsing", Loc: loc, Msg: "Input should be a valid boolean, unable to interpret input", Input: v}
	}
	return false, &mcpFieldErr{Type: "bool_type", Loc: loc, Msg: "Input should be a valid boolean", Input: v}
}

// ---- python str()/truthiness of decoded JSON, for PATCH values ----

func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	}
	return pyRepr(v)
}

func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyStrRepr(x)
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			return s
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return s
		}
		out := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(out, ".") {
			out += ".0"
		}
		return out
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyStrRepr(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// ---- path/query parameters ----

// lastQuery returns the last value of a repeated query parameter, like Starlette.
func lastQuery(q url.Values, name string) (string, bool) {
	vs, ok := q[name]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[len(vs)-1], true
}

// scimIntParam parses an int query/path parameter with optional bounds.
// ge/le are optional inclusive bounds.
func scimIntParam(raw string, loc []any, ge, le *int64) (int64, *mcpFieldErr) {
	n, e := pyInt(raw, loc)
	if e != nil {
		return 0, e
	}
	v := n.v
	if n.huge {
		v = math.MaxInt64
	}
	if ge != nil && v < *ge {
		return 0, &mcpFieldErr{Type: "greater_than_equal", Loc: loc, Msg: fmt.Sprintf("Input should be greater than or equal to %d", *ge), Input: raw, Ctx: map[string]any{"ge": *ge}}
	}
	if le != nil && v > *le {
		return 0, &mcpFieldErr{Type: "less_than_equal", Loc: loc, Msg: fmt.Sprintf("Input should be less than or equal to %d", *le), Input: raw, Ctx: map[string]any{"le": *le}}
	}
	return v, nil
}

func writeFieldErrs(w http.ResponseWriter, errs []mcpFieldErr) {
	httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": errs})
}
