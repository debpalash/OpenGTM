package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destvalidate"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Destination CRUD (routers/destinations.py): list, create, patch and delete. Like FastAPI the order is authentication,
// workspace, role (403), body validation (422), then the lookups (404) and the handler's own checks.

const (
	destNotFound      = "Destination not found"
	destDuplicateName = "A destination with this name already exists for the audience"
	destSecretsOff    = "Secrets are not configured"
)

// destCrudMaxBody bounds the JSON body (Python has no cap; documented deviation).
var destCrudMaxBody int64 = 1 << 20

// registerDestsCrud registers GET/POST /api/audience-destinations and PATCH/DELETE /api/audience-destinations/{destination_id}.
func registerDestsCrud(r *httpx.Router, d Deps) {
	h := destCrud{d: d}
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("activation.write", "editor", "admin")
	r.Handle("GET /api/audience-destinations", http.HandlerFunc(h.list), authn, workspace)
	r.Handle("POST /api/audience-destinations", http.HandlerFunc(h.create), authn, workspace, editor)
	r.Handle("PATCH /api/audience-destinations/{destination_id}", http.HandlerFunc(h.patch), authn, workspace, editor)
	r.Handle("DELETE /api/audience-destinations/{destination_id}", http.HandlerFunc(h.remove), authn, workspace, editor)
}

type destCrud struct{ d Deps }

func (h destCrud) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

func (h destCrud) ws(r *http.Request) string {
	wc, _ := identity.WorkspaceFrom(r.Context())
	return wc.WorkspaceID
}

// internal answers 500 and logs only the error's type: store and secret errors can carry config or credential text.
func (h destCrud) internal(w http.ResponseWriter, what string, err error) {
	h.log().Error("destinations: "+what+" failed", "err_type", fmt.Sprintf("%T", err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h destCrud) body(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, destCrudMaxBody+1))
	if err != nil || int64(len(raw)) > destCrudMaxBody {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	return raw, true
}

// invalid answers a body decode error: a FastAPI 422 list, anything else a 500.
func (h destCrud) invalid(w http.ResponseWriter, err error) {
	var v *destvalidate.ValidationError
	if errors.As(err, &v) {
		httpx.ValidationError(w, v.Errors...)
		return
	}
	h.internal(w, "decode", err)
}

// destIDOK reports whether a path id can exist: Postgres cannot store NUL or invalid UTF-8, so such an id matches nothing.
func destIDOK(id string) bool { return !strings.ContainsRune(id, 0) && utf8.ValidString(id) }

// destISO is a naive datetime as FastAPI renders it: microseconds only when non-zero.
func destISO(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

func destISOPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := destISO(*t)
	return &s
}

// destRunJSON is DestinationRun.to_api().
type destRunJSON struct {
	ID            string  `json:"id"`
	DestinationID string  `json:"destination_id"`
	Status        string  `json:"status"`
	Attempted     int     `json:"attempted"`
	Succeeded     int     `json:"succeeded"`
	Failed        int     `json:"failed"`
	Skipped       int     `json:"skipped"`
	Error         *string `json:"error"`
	StartedAt     *string `json:"started_at"`
	FinishedAt    *string `json:"finished_at"`
	CreatedAt     string  `json:"created_at"`
}

// destJSON is AudienceDestination.to_api(); its key order is the Python dict order.
type destJSON struct {
	ID              string          `json:"id"`
	AudienceID      string          `json:"audience_id"`
	Name            string          `json:"name"`
	DestinationType string          `json:"destination_type"`
	Enabled         bool            `json:"enabled"`
	Config          json.RawMessage `json:"config"`
	FieldMap        json.RawMessage `json:"field_map"`
	HealthStatus    string          `json:"health_status"`
	LastError       *string         `json:"last_error"`
	LastSuccessAt   *string         `json:"last_success_at"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
}

// destListJSON is a list entry: to_api() plus latest_run (null without runs).
type destListJSON struct {
	destJSON
	LatestRun *destRunJSON `json:"latest_run"`
}

func destView(d deststore.Destination) destJSON {
	fm := bytes.TrimSpace(d.FieldMap)
	if len(fm) == 0 || fm[0] != '{' { // `self.field_map or {}`: a stored JSON null answers {}
		fm = []byte(`{}`)
	}
	fm = pyJSONText(string(fm), `{}`) // decode + re-dump like FastAPI
	return destJSON{ID: d.ID, AudienceID: d.AudienceID, Name: d.Name, DestinationType: d.Type, Enabled: d.Enabled,
		Config: pyJSONText(string(d.SafeConfig()), `{}`), FieldMap: fm, HealthStatus: d.HealthStatus, LastError: d.LastError,
		LastSuccessAt: destISOPtr(d.LastSuccessAt), CreatedAt: destISO(d.CreatedAt), UpdatedAt: destISO(d.UpdatedAt)}
}

func destRunView(r deststore.Run) *destRunJSON {
	return &destRunJSON{ID: r.ID, DestinationID: r.DestinationID, Status: r.Status, Attempted: r.Attempted, Succeeded: r.Succeeded,
		Failed: r.Failed, Skipped: r.Skipped, Error: r.Error, StartedAt: destISOPtr(r.StartedAt), FinishedAt: destISOPtr(r.FinishedAt),
		CreatedAt: destISO(r.CreatedAt)}
}

func (h destCrud) list(w http.ResponseWriter, r *http.Request) {
	ws := h.ws(r)
	out := []destListJSON{}
	audienceID := r.URL.Query().Get("audience_id") // empty means no filter, as Python's `if audience_id:`
	if !destIDOK(audienceID) {
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		rows, err := deststore.ListDestinationsWithLatestRun(r.Context(), tx, audienceID)
		for _, row := range rows {
			item := destListJSON{destJSON: destView(row.Destination)}
			if row.LatestRun != nil {
				item.LatestRun = destRunView(*row.LatestRun)
			}
			out = append(out, item)
		}
		return err
	})
	if err != nil {
		h.internal(w, "list", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// destOrdered re-serializes a JSON object compactly with the client's key order (a duplicate key keeps its first
// position and last value), which the JSON config / field_map columns then store.
func destOrdered(raw json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return json.RawMessage(`{}`)
	}
	b, err := audParseBody(t)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range b.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		var c bytes.Buffer
		if json.Compact(&c, b.vals[k]) != nil {
			c.Reset()
			c.Write(b.vals[k])
		}
		buf.Write(c.Bytes())
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// destRawField is the raw value of a top-level key of a body already validated as an object.
func destRawField(body []byte, key string) (json.RawMessage, bool) {
	b, err := audParseBody(body)
	if err != nil {
		return nil, false
	}
	v, ok := b.vals[key]
	return v, ok
}

// destSecretField is the config key holding a workspace-secret reference for a type, "" when the type has none.
func destSecretField(typ string) string {
	switch typ {
	case "warehouse_http":
		return "header_secret_ref"
	case "slack":
		return "webhook_secret_ref"
	case "webhook":
		return "header_secret_ref" // optional: only checked when set
	}
	return ""
}

// outboundPolicy is the R2 policy for references a destination sends to its configured URL.
func (h destCrud) outboundPolicy() secrets.OutboundPolicy {
	return h.d.outboundPolicy()
}

// destCheckConfig is _validate_config plus, for the types with a secret reference, the get_secret lookup. A refusal is the
// 422 detail string; a non-nil error is an internal failure (secrets unavailable, decrypt failure).
func (h destCrud) checkConfig(ctx context.Context, ws, typ string, cfg map[string]any) (refusal string, err error) {
	if _, verr := destvalidate.ValidateConfig(typ, cfg); verr != nil {
		return verr.Error(), nil
	}
	field := destSecretField(typ)
	if field == "" {
		return "", nil
	}
	notFound := typ + " " + field + " was not found"
	if typ == "webhook" && !pyDestTruthy(cfg[field]) { // the header secret is optional on a webhook
		return "", nil
	}
	// R2: the reference must be in the outbound namespace (and not a provider/CRM credential). A refused name and a
	// missing one share one text, so the response is no oracle for which secrets exist.
	ref, isStr := cfg[field].(string)
	if !isStr || destdeliver.CheckRef(typ, ref, h.outboundPolicy(), nil) != nil {
		return notFound, nil
	}
	if h.d.Envelope == nil { // no master key: stored secrets cannot be decrypted
		return "", errSecretsOff
	}
	res := secrets.NewResolver(h.d.Envelope, secrets.NewPgStore(h.d.Pool), h.d.getenv())
	v, src, gerr := res.Get(ctx, ws, ref)
	if gerr != nil {
		return "", gerr
	}
	// Workspace secrets only (deviation from Python's get_secret fallback, logged for #114): the value is sent to a
	// destination-configured URL, so a global or env key must not be reachable by name.
	if v == "" || src != secrets.SourceWorkspace {
		return notFound, nil
	}
	return "", nil
}

// pyDestTruthy is Python truthiness for a decoded JSON config value.
func pyDestTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

var errSecretsOff = errors.New("destinations: secrets are not configured")

func (h destCrud) configFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, errSecretsOff) {
		httpx.Detail(w, http.StatusServiceUnavailable, destSecretsOff)
		return
	}
	h.internal(w, "secret lookup", err)
}

func (h destCrud) create(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, err := destvalidate.DecodeCreate(raw)
	if err != nil {
		h.invalid(w, err)
		return
	}
	ws := h.ws(r)
	ctx := r.Context()
	audNone := func() { httpx.Detail(w, http.StatusNotFound, audNotFound) }
	if !destIDOK(in.AudienceID) {
		audNone()
		return
	}
	// 1. the audience (own transaction: the secret lookup below takes a second pool connection).
	err = tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) error {
		_, err := gen.New(tx).AudCrudGet(ctx, gen.AudCrudGetParams{ID: in.AudienceID, WorkspaceID: ws})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		audNone()
		return
	}
	if err != nil {
		h.internal(w, "audience lookup", err)
		return
	}
	// 2. the config, then 3. the secret reference.
	refusal, err := h.checkConfig(ctx, ws, in.DestinationType, in.Config)
	if err != nil {
		h.configFailed(w, err)
		return
	}
	if refusal != "" {
		httpx.Detail(w, http.StatusUnprocessableEntity, refusal)
		return
	}
	cfgRaw, _ := destRawField(raw, "config")
	fmRaw, _ := destRawField(raw, "field_map")
	// 4. the insert: the unique (workspace, audience, name) constraint is the duplicate check.
	var created deststore.Destination
	err = tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) error {
		var err error
		created, err = deststore.CreateDestination(ctx, tx, deststore.CreateInput{AudienceID: in.AudienceID, Name: in.Name,
			Type: in.DestinationType, Enabled: in.Enabled, Config: destOrdered(cfgRaw), FieldMap: destOrdered(fmRaw)})
		return err
	})
	switch {
	case errors.Is(err, deststore.ErrDuplicate):
		httpx.Detail(w, http.StatusConflict, destDuplicateName)
	case errors.Is(err, deststore.ErrAudienceNotFound):
		audNone()
	case err != nil:
		h.internal(w, "create", err)
	default:
		httpx.WriteJSON(w, http.StatusCreated, destView(created))
	}
}

// destNullField is the 422 pydantic gives a null for a non-Optional field (Python lets the null through to the database
// and answers 500; documented deviation).
func destNullField(field, typ, msg string) httpx.FieldError {
	return httpx.FieldError{Type: typ, Loc: []any{"body", field}, Msg: msg, Input: nil}
}

func (h destCrud) patch(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	in, err := destvalidate.DecodePatch(raw)
	if err != nil {
		h.invalid(w, err)
		return
	}
	var nulls []httpx.FieldError
	if in.Name.Null {
		nulls = append(nulls, destNullField("name", "string_type", "Input should be a valid string"))
	}
	if in.Enabled.Null {
		nulls = append(nulls, destNullField("enabled", "bool_type", "Input should be a valid boolean"))
	}
	if len(nulls) > 0 {
		httpx.ValidationError(w, nulls...)
		return
	}
	id := r.PathValue("destination_id")
	if !destIDOK(id) {
		httpx.Detail(w, http.StatusNotFound, destNotFound)
		return
	}
	ws := h.ws(r)
	ctx := r.Context()
	up := deststore.UpdateInput{}
	if in.Name.Set {
		v := in.Name.Val
		up.Name = &v
	}
	if in.Enabled.Set {
		v := in.Enabled.Val
		up.Enabled = &v
	}
	if in.FieldMap.Set {
		up.FieldMap = []byte(`{}`) // null clears it (Python stores JSON null and answers {})
		if !in.FieldMap.Null {
			rawFM, _ := destRawField(raw, "field_map")
			up.FieldMap = destOrdered(rawFM)
		}
	}
	if in.Config.Set {
		// The lookup comes first (404), then the stored type decides the validation.
		var cur deststore.Destination
		err := tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) error {
			var err error
			cur, err = deststore.GetDestination(ctx, tx, id)
			return err
		})
		if errors.Is(err, deststore.ErrNotFound) {
			httpx.Detail(w, http.StatusNotFound, destNotFound)
			return
		}
		if err != nil {
			h.internal(w, "lookup", err)
			return
		}
		cfg := in.Config.Val
		if in.Config.Null || cfg == nil { // `changes["config"] or {}`
			cfg = map[string]any{}
		}
		refusal, err := h.checkConfig(ctx, ws, cur.Type, cfg)
		if err != nil {
			h.configFailed(w, err)
			return
		}
		if refusal != "" {
			httpx.Detail(w, http.StatusUnprocessableEntity, refusal)
			return
		}
		up.Config = []byte(`{}`)
		if !in.Config.Null {
			rawCfg, _ := destRawField(raw, "config")
			up.Config = destOrdered(rawCfg)
		}
	}
	var updated deststore.Destination
	err = tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) error {
		cur, err := deststore.GetDestination(ctx, tx, id)
		if err != nil {
			return err
		}
		// SQLAlchemy flushes (and bumps updated_at) only for attributes whose value actually changed: send just those.
		if up.Name != nil && *up.Name == cur.Name {
			up.Name = nil
		}
		if up.Enabled != nil && *up.Enabled == cur.Enabled {
			up.Enabled = nil
		}
		if up.Config != nil && audJSONEqual(up.Config, cur.Config) {
			up.Config = nil
		}
		if up.FieldMap != nil && audJSONEqual(up.FieldMap, cur.FieldMap) {
			up.FieldMap = nil
		}
		updated, err = deststore.UpdateDestination(ctx, tx, id, up)
		return err
	})
	switch {
	case errors.Is(err, deststore.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, destNotFound)
	case errors.Is(err, deststore.ErrDuplicate): // Python: uncaught IntegrityError -> 500 (D3)
		httpx.Detail(w, http.StatusConflict, destDuplicateName)
	case err != nil:
		h.internal(w, "patch", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, destView(updated))
	}
}

func (h destCrud) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("destination_id")
	if !destIDOK(id) {
		httpx.Detail(w, http.StatusNotFound, destNotFound)
		return
	}
	ws := h.ws(r)
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, ws, func(tx pgx.Tx) error {
		return deststore.DeleteDestination(r.Context(), tx, id)
	})
	if errors.Is(err, deststore.ErrNotFound) {
		httpx.Detail(w, http.StatusNotFound, destNotFound)
		return
	}
	if err != nil {
		h.internal(w, "delete", err)
		return
	}
	w.Header().Set("Content-Type", "application/json") // FastAPI keeps it on a 204
	w.WriteHeader(http.StatusNoContent)
}
