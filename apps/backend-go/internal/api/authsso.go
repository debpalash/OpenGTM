package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govidp"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Workspace SSO login (S-M5), ported from Python routers/auth.py /auth/sso/*:
//
//	GET /auth/sso/{slug}           {"enabled", "workspace"} (404 Workspace not found)
//	GET /auth/sso/{slug}/login     302 to the IdP: authorization code + PKCE (S256), state and nonce
//	GET /auth/sso/{slug}/callback  303 to /login#sso_access_token=... or /login#sso_error=...
//
// state, nonce, the PKCE verifier, the workspace, the issuer and the redirect URI travel in a
// short-lived HMAC-signed HttpOnly cookie scoped to /auth/sso/. The callback refuses a missing,
// forged, expired or other-workspace cookie and a state mismatch; the ID token must name the
// stored issuer exactly. Identity linking is govidp.ResolveSSOUser: by (workspace, issuer,
// subject) binding, else only to a non-privileged member of this workspace.

const (
	ssoCookieName = "opengtm_sso_flow"
	ssoCookiePath = "/auth/sso/"
	ssoFlowTTL    = 10 * time.Minute
	ssoGeneric    = "SSO authentication failed"
)

// ssoHTTPClient builds the client for discovery, token and JWKS calls (tests replace it). The
// issuer is operator-approved (SSO_ALLOWED_ISSUERS), so it is the operator egress client, but
// with its own allow-list: only SSO_EGRESS_ALLOW_HOSTS admits a self-hosted IdP on a private
// address (empty by default; EGRESS_ALLOW_HOSTS does not widen it).
var ssoHTTPClient = func(d Deps) *http.Client {
	c := egress.NewOperatorClient(strings.Split(d.Flags.Governance.SSOEgressAllowHosts, ","))
	c.Timeout = 15 * time.Second
	return c
}

// ssoNow is the callback clock (ID token exp/iat, cookie expiry).
var ssoNow = time.Now

type ssoLogin struct {
	d   Deps
	hc  *http.Client
	key []byte
}

func registerSSOLogin(r *httpx.Router, d Deps) {
	m := hmac.New(sha256.New, d.Signer.Secret)
	m.Write([]byte("opengtm/sso-flow/v1"))
	h := &ssoLogin{d: d, hc: ssoHTTPClient(d), key: m.Sum(nil)}
	r.Handle("GET /auth/sso/{workspace_slug}", http.HandlerFunc(h.status))
	r.Handle("GET /auth/sso/{workspace_slug}/login", http.HandlerFunc(h.login))
	r.Handle("GET /auth/sso/{workspace_slug}/callback", http.HandlerFunc(h.callback))
}

func (h *ssoLogin) workspace(w http.ResponseWriter, r *http.Request) (gen.Workspace, bool) {
	ws, err := gen.New(h.d.Pool).WorkspaceBySlug(r.Context(), r.PathValue("workspace_slug"))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Detail(w, http.StatusNotFound, "Workspace not found")
		return ws, false
	}
	if err != nil {
		h.d.logger().Error("auth sso: workspace lookup failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return ws, false
	}
	return ws, true
}

func (h *ssoLogin) config(ctx context.Context, ws string) (cfg govidp.OIDCConfig, err error) {
	err = tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) error {
		cfg, err = govidp.GetConfig(ctx, tx, ws)
		return err
	})
	return cfg, err
}

func (h *ssoLogin) status(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.workspace(w, r)
	if !ok {
		return
	}
	cfg, err := h.config(r.Context(), ws.ID)
	if err != nil {
		h.d.logger().Error("auth sso: config read failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": cfg.Enabled, "workspace": ws.Name})
}

// ready loads the enabled, allow-listed config and the client secret. A refusal is a
// *govidp.ValueError (Python's client_for ValueError).
func (h *ssoLogin) ready(ctx context.Context, ws string) (govidp.OIDCConfig, string, error) {
	cfg, err := h.config(ctx, ws)
	if err != nil {
		return cfg, "", err
	}
	if !cfg.Enabled {
		return cfg, "", &govidp.ValueError{Msg: "Workspace SSO is not enabled"}
	}
	if _, err := govidp.ValidateIssuer(cfg.Issuer); err != nil {
		return cfg, "", err
	}
	if h.d.Envelope == nil {
		return cfg, "", &govidp.ValueError{Msg: "Workspace SSO client secret is not configured"}
	}
	res := secrets.NewResolver(h.d.Envelope, secrets.NewPgStore(h.d.Pool), h.d.getenv())
	secret, _, err := res.Get(ctx, ws, govidp.SecretKey)
	if err != nil {
		return cfg, "", errors.New("auth sso: reading client secret failed")
	}
	if secret == "" {
		return cfg, "", &govidp.ValueError{Msg: "Workspace SSO client secret is not configured"}
	}
	return cfg, secret, nil
}

func (h *ssoLogin) redirectBase(r *http.Request) string {
	if b := h.d.Flags.Governance.SSORedirectBaseURL; b != "" {
		return b
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type ssoFlowCookie struct {
	WS       string `json:"w"`
	Slug     string `json:"s"`
	Issuer   string `json:"i"`
	Redirect string `json:"r"`
	State    string `json:"st"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Exp      int64  `json:"e"`
}

func (h *ssoLogin) mac(payload string) string {
	m := hmac.New(sha256.New, h.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (h *ssoLogin) setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: ssoCookieName, Value: value, Path: ssoCookiePath, MaxAge: maxAge,
		HttpOnly: true, Secure: strings.HasPrefix(h.redirectBase(r), "https://"), SameSite: http.SameSiteLaxMode})
}

func (h *ssoLogin) readCookie(r *http.Request) (ssoFlowCookie, bool) {
	var f ssoFlowCookie
	c, err := r.Cookie(ssoCookieName)
	if err != nil {
		return f, false
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(h.mac(payload))) {
		return f, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(raw, &f) != nil || ssoNow().Unix() >= f.Exp {
		return ssoFlowCookie{}, false
	}
	return f, true
}

func noCacheRedirect(w http.ResponseWriter, r *http.Request, to string, code int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, to, code)
}

func (h *ssoLogin) login(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.workspace(w, r)
	if !ok {
		return
	}
	cfg, _, err := h.ready(r.Context(), ws.ID)
	var ve *govidp.ValueError
	if errors.As(err, &ve) {
		httpx.Detail(w, http.StatusNotFound, ve.Msg)
		return
	}
	if err != nil {
		h.d.logger().Error("auth sso: login setup failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	disc, err := govidp.Discover(r.Context(), h.hc, cfg.Issuer)
	if err != nil {
		h.d.logger().Warn("auth sso: discovery failed", "workspace", ws.ID)
		httpx.Detail(w, http.StatusBadGateway, "SSO provider unavailable")
		return
	}
	flow, err := govidp.NewFlow()
	if err != nil {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	redirect := h.redirectBase(r) + "/auth/sso/" + url.PathEscape(ws.Slug) + "/callback"
	raw, _ := json.Marshal(ssoFlowCookie{WS: ws.ID, Slug: ws.Slug, Issuer: cfg.Issuer, Redirect: redirect,
		State: flow.State, Nonce: flow.Nonce, Verifier: flow.Verifier, Exp: ssoNow().Add(ssoFlowTTL).Unix()})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	h.setCookie(w, r, payload+"."+h.mac(payload), int(ssoFlowTTL/time.Second))
	noCacheRedirect(w, r, disc.AuthURL(cfg.ClientID, redirect, flow), http.StatusFound)
}

// ssoFail is the Python error redirect: /login#sso_error=<quoted message, at most 200 chars>.
func ssoFail(w http.ResponseWriter, r *http.Request, msg string) {
	if utf8.RuneCountInString(msg) > 200 {
		msg = string([]rune(msg)[:200])
	}
	noCacheRedirect(w, r, "/login#sso_error="+url.PathEscape(msg), http.StatusSeeOther)
}

func (h *ssoLogin) callback(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.workspace(w, r)
	if !ok {
		return
	}
	flow, haveFlow := h.readCookie(r)
	h.setCookie(w, r, "", -1) // single use: cleared whatever happens next
	q := r.URL.Query()
	state := q.Get("state")
	if !haveFlow || flow.WS != ws.ID || flow.Slug != ws.Slug || state == "" ||
		subtle.ConstantTimeCompare([]byte(state), []byte(flow.State)) != 1 {
		ssoFail(w, r, ssoGeneric)
		return
	}
	code := q.Get("code")
	if q.Get("error") != "" || code == "" {
		ssoFail(w, r, ssoGeneric)
		return
	}
	ctx := r.Context()
	cfg, secret, err := h.ready(ctx, ws.ID)
	var ve *govidp.ValueError
	if errors.As(err, &ve) {
		ssoFail(w, r, ve.Msg)
		return
	}
	if err != nil || cfg.Issuer != flow.Issuer { // config changed while the user was at the IdP
		h.callbackFailed(ws.ID, err)
		ssoFail(w, r, ssoGeneric)
		return
	}
	disc, err := govidp.Discover(ctx, h.hc, cfg.Issuer)
	if err != nil {
		h.callbackFailed(ws.ID, err)
		ssoFail(w, r, ssoGeneric)
		return
	}
	claims, err := disc.Exchange(ctx, h.hc, cfg.ClientID, secret, flow.Redirect, code,
		govidp.Flow{State: flow.State, Nonce: flow.Nonce, Verifier: flow.Verifier}, ssoNow())
	if err != nil {
		h.callbackFailed(ws.ID, err)
		ssoFail(w, r, ssoGeneric)
		return
	}
	email := govidp.NormalizeEmail(claims.Email)
	if claims.Subject == "" || email == "" || !claims.EmailVerified {
		ssoFail(w, r, "The identity provider must return a verified email")
		return
	}
	domain := ""
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		domain = email[i+1:]
	}
	if len(cfg.AllowedDomains) > 0 && !containsStr(cfg.AllowedDomains, domain) {
		ssoFail(w, r, "Email domain is not allowed for this workspace")
		return
	}
	var uid int32
	err = tenancy.InWorkspace(ctx, h.d.Pool, ws.ID, func(tx pgx.Tx) (err error) {
		uid, err = govidp.ResolveSSOUser(ctx, tx, ws.ID, cfg.Issuer, claims.Subject, email, cfg, func() (string, error) {
			pw, err := govidp.NewFlow() // 32 random bytes nobody knows: an SSO-only account
			if err != nil {
				return "", err
			}
			return auth.HashPassword(pw.Verifier)
		})
		return err
	})
	if errors.As(err, &ve) {
		ssoFail(w, r, ve.Msg)
		return
	}
	if err != nil {
		h.callbackFailed(ws.ID, err)
		ssoFail(w, r, ssoGeneric)
		return
	}
	u, err := h.d.Identity.ByID(ctx, uid)
	if err != nil || !u.IsActive {
		ssoFail(w, r, govidp.MsgSSOInactive)
		return
	}
	// The session is scoped to this workspace (sso_ws): the workspace's IdP vouches for this
	// workspace only, so the token is refused for any other one (identity.Authenticate/Workspace).
	access, err := h.d.Signer.AccessFor(u.Username, []string{"sso"}, ws.ID)
	if err == nil {
		err = h.d.Identity.TouchLastLogin(ctx, uid)
	}
	if err == nil {
		err = gen.New(h.d.Pool).SetActiveWorkspace(ctx, gen.SetActiveWorkspaceParams{UserID: uid, WorkspaceID: ws.ID})
	}
	if err != nil {
		h.callbackFailed(ws.ID, err)
		ssoFail(w, r, ssoGeneric)
		return
	}
	noCacheRedirect(w, r, "/login#sso_access_token="+url.PathEscape(access), http.StatusSeeOther)
}

func (h *ssoLogin) callbackFailed(ws string, err error) {
	h.d.logger().Warn("auth sso: callback failed", "workspace", ws, "err_type", errTypeName(err))
}

func containsStr(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
