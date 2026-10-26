package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govadmin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govidp"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type ssoRoutes struct{ d Deps }

const ssoSecretsOff = "Secrets are not configured"

// registerGovernanceSSO registers the sso and scim-token routes (Task 15). All five need governance.manage
// (admin), as Python's require_admin. Audit events come from the global audit middleware.
func registerGovernanceSSO(r *httpx.Router, d Deps) {
	h := ssoRoutes{d: d}
	mw := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity), govadmin.GateAdmin("governance.manage")}
	r.Handle("GET /api/governance/sso", http.HandlerFunc(h.getSSO), mw...)
	r.Handle("PUT /api/governance/sso", http.HandlerFunc(h.putSSO), mw...)
	r.Handle("GET /api/governance/scim-token", http.HandlerFunc(h.getToken), mw...)
	r.Handle("POST /api/governance/scim-token", http.HandlerFunc(h.rotateToken), mw...)
	r.Handle("DELETE /api/governance/scim-token", http.HandlerFunc(h.revokeToken), mw...)
}

// fail never logs request data (client secrets, tokens); only the operation and error type.
func (h ssoRoutes) fail(w http.ResponseWriter, op string, err error) {
	h.d.logger().Error("governance sso: request failed", "op", op, "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (h ssoRoutes) ctx(w http.ResponseWriter, r *http.Request) (identity.WorkspaceCtx, bool) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
	}
	return c, ok
}

// resolver is nil when no master key is configured: the client secret can then be neither stored nor read.
func (h ssoRoutes) resolver() *secrets.Resolver {
	if h.d.Envelope == nil {
		return nil
	}
	return secrets.NewResolver(h.d.Envelope, secrets.NewPgStore(h.d.Pool), h.d.getenv())
}

// ssoResponse is {**config, "owner_password_fallback": true, "client_secret_configured": bool}; the secret itself
// is never part of it.
type ssoResponse struct {
	govidp.OIDCConfig
	OwnerPasswordFallback  bool `json:"owner_password_fallback"`
	ClientSecretConfigured bool `json:"client_secret_configured"`
}

func (h ssoRoutes) getSSO(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var cfg govidp.OIDCConfig
	if err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		cfg, err = govidp.GetConfig(r.Context(), tx, c.WorkspaceID)
		return err
	}); err != nil {
		h.fail(w, "get_sso", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ssoResponse{cfg, true, h.configured(r, c.WorkspaceID)})
}

func (h ssoRoutes) configured(r *http.Request, ws string) bool {
	res := h.resolver()
	return res != nil && govidp.SecretConfigured(r.Context(), res, ws)
}

// parseOIDCInput validates the OidcUpdate body in pydantic lax mode, collecting every error in field order.
func parseOIDCInput(raw []byte) (govidp.OIDCInput, []httpx.FieldError) {
	in := govidp.OIDCInput{DefaultRole: "viewer", AllowedDomains: []string{}}
	o, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return in, errs
	}
	boolField := func(name string, dst *bool) {
		if v, ok := o.Get(name); ok {
			b, e := lenientBool(v, []any{"body", name})
			*dst, errs = b, append(errs, e...)
		}
	}
	strField := func(name string, dst *string) {
		if v, ok := o.Get(name); ok {
			s, isStr := v.(string)
			if !isStr {
				errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", name}, Msg: "Input should be a valid string", Input: v})
				return
			}
			*dst = s
		}
	}
	boolField("enabled", &in.Enabled)
	boolField("enforce_sso", &in.EnforceSSO)
	strField("issuer", &in.Issuer)
	strField("client_id", &in.ClientID)
	strField("client_secret", &in.ClientSecret)
	if v, ok := o.Get("allowed_domains"); ok {
		list, isList := v.([]any)
		if !isList {
			errs = append(errs, httpx.FieldError{Type: "list_type", Loc: []any{"body", "allowed_domains"}, Msg: "Input should be a valid list", Input: v})
		}
		for i, item := range list {
			s, isStr := item.(string)
			if !isStr {
				errs = append(errs, httpx.FieldError{Type: "string_type", Loc: []any{"body", "allowed_domains", i}, Msg: "Input should be a valid string", Input: item})
				continue
			}
			in.AllowedDomains = append(in.AllowedDomains, s)
		}
	}
	boolField("auto_provision", &in.AutoProvision)
	if v, ok := o.Get("default_role"); ok {
		s, _ := v.(string)
		switch s {
		case "viewer", "member", "editor":
			in.DefaultRole = s
		default:
			const expected = "'viewer', 'member' or 'editor'"
			errs = append(errs, httpx.FieldError{Type: "literal_error", Loc: []any{"body", "default_role"},
				Msg: "Input should be " + expected, Input: v, Ctx: map[string]any{"expected": expected}})
		}
	}
	return in, errs
}

func (h ssoRoutes) putSSO(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	raw, ok := wbcrud.ReadBody(r.Body)
	if !ok {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	in, errs := parseOIDCInput(raw)
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	res := h.resolver()
	if res == nil {
		httpx.Detail(w, http.StatusServiceUnavailable, ssoSecretsOff)
		return
	}
	var cfg govidp.OIDCConfig
	// The client secret is written by the resolver (its own store/connection) before the config row, as Python does;
	// a failed config write after a good secret write leaves the secret, which is Python's order too.
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		cfg, err = govidp.SaveConfig(r.Context(), tx, res, c.WorkspaceID, in)
		return err
	})
	if err != nil {
		if ve, isVE := err.(*govidp.ValueError); isVE {
			httpx.Detail(w, http.StatusUnprocessableEntity, ve.Msg)
			return
		}
		h.fail(w, "put_sso", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ssoResponse{cfg, true, govidp.SecretConfigured(r.Context(), res, c.WorkspaceID)})
}

// scimStatusBody is {"configured", "base_path", **status}: a nil status adds nothing.
type scimStatusBody struct {
	Configured bool   `json:"configured"`
	BasePath   string `json:"base_path"`
	*govidp.TokenStatus
}

// scimRotateBody is {"token", "base_path", "warning", **status}.
type scimRotateBody struct {
	Token    string `json:"token"`
	BasePath string `json:"base_path"`
	Warning  string `json:"warning"`
	*govidp.TokenStatus
}

func scimBasePath(c identity.WorkspaceCtx) string { return "/scim/v2/" + c.Slug }

func (h ssoRoutes) getToken(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var st *govidp.TokenStatus
	if err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		st, err = govidp.Status(r.Context(), tx, c.WorkspaceID, time.Now())
		return err
	}); err != nil {
		h.fail(w, "get_scim_token", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, scimStatusBody{Configured: st != nil && !st.Expired, BasePath: scimBasePath(c), TokenStatus: st})
}

func (h ssoRoutes) rotateToken(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var token string
	var st *govidp.TokenStatus
	if err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) (err error) {
		now := time.Now()
		if token, err = govidp.Rotate(r.Context(), tx, c.WorkspaceID, c.User.ID, now); err != nil {
			return err
		}
		st, err = govidp.Status(r.Context(), tx, c.WorkspaceID, now)
		return err
	}); err != nil {
		h.fail(w, "rotate_scim_token", err)
		return
	}
	// The plaintext token is returned exactly once and is never logged or stored.
	httpx.WriteJSON(w, http.StatusCreated, scimRotateBody{Token: token, BasePath: scimBasePath(c),
		Warning: "Copy this token now; it cannot be retrieved again.", TokenStatus: st})
}

func (h ssoRoutes) revokeToken(w http.ResponseWriter, r *http.Request) {
	c, ok := h.ctx(w, r)
	if !ok {
		return
	}
	if err := tenancy.InWorkspace(r.Context(), h.d.Pool, c.WorkspaceID, func(tx pgx.Tx) error {
		return govidp.Revoke(r.Context(), tx, c.WorkspaceID)
	}); err != nil {
		h.fail(w, "revoke_scim_token", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNoContent)
}
