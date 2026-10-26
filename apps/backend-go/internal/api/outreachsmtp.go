package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpmail"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Hardening bounds (D2). Python stores any string and any integer; Go answers a pydantic-shaped 422 instead, and
// limits the test route to 10 requests a minute per workspace.
const (
	smtpHostMax     = 253
	smtpEmailMax    = 320
	smtpFromNameMax = 200
	smtpPasswordMax = 1024
	smtpMaxPerHour  = 100000
	smtpTestRate    = "10/minute"
	smtpTestTimeout = 30 * time.Second
	smtpErrTextMax  = 300
	smtpSecretsOff  = "Secrets are not configured"
)

var smtpHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// smtpMailerFor builds the mailer behind POST /smtp/test: the egress dial guard (loopback, private and metadata
// addresses refused unless the operator allow-listed the host outside production) and verified TLS. Tests replace it.
var smtpMailerFor = func(d Deps) outreachsend.Mailer {
	return &outreachsend.SMTPMailer{Client: &smtpclient.Client{}, Dial: egress.NewDialContext(d.Config.AppEnv, d.EgressAllowHosts)}
}

type smtpRoutes struct{ seqRoutes }

// registerOutreachSMTP registers the 3 SMTP routes (Task 22). Status needs any member (current_workspace); config
// and test need outreach.write (admin).
func registerOutreachSMTP(r *httpx.Router, d Deps) {
	h := smtpRoutes{seqRoutes{d: d}}
	authn := identity.Authenticate(d.Signer, d.Identity)
	read := []httpx.Middleware{authn, identity.Workspace(d.Identity)}
	write := append(append([]httpx.Middleware{}, read...), identity.RequireRole("outreach.write", "admin"))
	test := write
	if d.Limiter != nil { // after authentication and the role gate; the key is the workspace (X-Workspace-Id)
		test = append(append([]httpx.Middleware{}, write...), d.Limiter.Limit(smtpTestRate))
	}
	r.Handle("GET /api/outreach/smtp/status", http.HandlerFunc(h.status), read...)
	r.Handle("PUT /api/outreach/smtp/config", http.HandlerFunc(h.update), write...)
	r.Handle("POST /api/outreach/smtp/test", http.HandlerFunc(h.test), test...)
}

func (h smtpRoutes) resolver(w http.ResponseWriter) (*secrets.Resolver, bool) {
	if h.d.Envelope == nil { // no master key: SMTP secrets can be neither stored nor decrypted
		httpx.Detail(w, http.StatusServiceUnavailable, smtpSecretsOff)
		return nil, false
	}
	return secrets.NewResolver(h.d.Envelope, secrets.NewPgStore(h.d.Pool), h.d.getenv()), true
}

// smtpStatus is the response of GET /smtp/status in Python's key order. The password is not a field.
type smtpStatus struct {
	Configured bool    `json:"configured"`
	Host       *string `json:"host"`
	Email      *string `json:"email"`
	FromName   string  `json:"from_name"`
	MaxPerHour int     `json:"max_per_hour"`
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (h smtpRoutes) status(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	res, ok := h.resolver(w)
	if !ok {
		return
	}
	cfg := outreachcfg.LoadSMTP(r.Context(), res, ws)
	if cfg.Port == 0 { // a failed lookup yields the zero config; Python's SMTPConfig() defaults apply
		cfg.FromName, cfg.MaxPerHour = "OpenGTM", 50
	}
	httpx.WriteJSON(w, http.StatusOK, smtpStatus{Configured: cfg.Configured(), Host: nilIfEmpty(cfg.Host), Email: nilIfEmpty(cfg.Email),
		FromName: cfg.FromName, MaxPerHour: cfg.MaxPerHour})
}

func smtpHost(v any, loc []any) (string, *httpx.FieldError) {
	s, e := seqString(v, loc, 1<<20) // length is checked after trimming, below
	if e != nil {
		return "", e
	}
	s = strings.TrimSpace(s)
	if s == "" { // blank is ignored, as set_secret does
		return "", nil
	}
	if len(s) > smtpHostMax || !(smtpHostRe.MatchString(s) || isBareIP(s)) {
		fe := seqFE("value_error", "Value error, smtp_host must be a bare host name or IP address (no scheme, port, path, credentials or spaces)", loc, v, nil)
		return "", &fe
	}
	return s, nil
}

func isBareIP(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Zone() == ""
}

// parseSMTPConfig validates SMTPConfigUpdate: every field optional, null means "leave unchanged". Errors come in
// declaration order.
func parseSMTPConfig(raw []byte) (outreachcfg.SMTPUpdate, []httpx.FieldError) {
	var u outreachcfg.SMTPUpdate
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return u, errs
	}
	get := func(k string) (any, bool) {
		v, present := obj.Get(k)
		return v, present && v != nil
	}
	add := func(e *httpx.FieldError) bool {
		if e != nil {
			errs = append(errs, *e)
		}
		return e == nil
	}
	if v, ok := get("smtp_host"); ok {
		if s, e := smtpHost(v, seqLoc("smtp_host")); add(e) {
			u.Host = &s
		}
	}
	if v, ok := get("smtp_port"); ok {
		if n, e := seqIntRange(v, seqLoc("smtp_port"), 1, 65535); add(e) {
			p := int(n)
			u.Port = &p
		}
	}
	if v, ok := get("smtp_email"); ok {
		if s, e := seqString(v, seqLoc("smtp_email"), smtpEmailMax); add(e) {
			u.Email = &s
		}
	}
	if v, ok := get("smtp_password"); ok {
		if s, e := seqString(v, seqLoc("smtp_password"), smtpPasswordMax); add(e) {
			p := config.Secret(s)
			u.Password = &p
		}
	}
	if v, ok := get("smtp_from_name"); ok {
		if s, e := seqString(v, seqLoc("smtp_from_name"), smtpFromNameMax); add(e) {
			u.FromName = &s
		}
	}
	if v, ok := get("smtp_max_per_hour"); ok {
		if n, e := seqIntRange(v, seqLoc("smtp_max_per_hour"), 1, smtpMaxPerHour); add(e) {
			p := int(n)
			u.MaxPerHour = &p
		}
	}
	if v, ok := get("smtp_use_tls"); ok {
		if b, e := watchBool(v, seqLoc("smtp_use_tls")); add(e) {
			u.UseTLS = &b
		}
	}
	if errs != nil {
		return outreachcfg.SMTPUpdate{}, errs
	}
	return u, nil
}

func (h smtpRoutes) update(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	u, errs := parseSMTPConfig(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	res, ok := h.resolver(w)
	if !ok {
		return
	}
	if err := outreachcfg.SetSMTP(r.Context(), res, ws, u); err != nil {
		h.fail(w, "smtp config", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// smtpErrText is the failure text shown to the admin: the classified send error with the password (and its
// base64 form, as it appears in an AUTH exchange) scrubbed, control characters dropped and the length capped.
func smtpErrText(err error, password string) string {
	t := outreachsend.SendErrorText(err)
	if len(password) >= 4 {
		t = strings.ReplaceAll(t, password, "[redacted]")
		t = strings.ReplaceAll(t, base64.StdEncoding.EncodeToString([]byte(password)), "[redacted]")
	}
	t = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, t)
	if rs := []rune(t); len(rs) > smtpErrTextMax {
		t = string(rs[:smtpErrTextMax])
	}
	return t
}

func (h smtpRoutes) test(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.ws(w, r)
	if !ok {
		return
	}
	raw, ok := h.body(w, r)
	if !ok {
		return
	}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	v, present := obj.Get("to_email")
	if !present {
		httpx.ValidationError(w, seqFE("missing", "Field required", seqLoc("to_email"), obj, nil))
		return
	}
	to, e := watchStr(v, seqLoc("to_email"))
	if e != nil {
		httpx.ValidationError(w, *e)
		return
	}
	res, ok := h.resolver(w)
	if !ok {
		return
	}
	cfg := outreachcfg.LoadSMTP(r.Context(), res, ws)
	if msg := cfg.ConfigError(); msg != "" { // workspace host without its own credentials: never fall back to the platform's
		httpx.Detail(w, http.StatusBadRequest, "SMTP not configured: "+msg)
		return
	}
	if !cfg.Configured() {
		httpx.Detail(w, http.StatusBadRequest, "SMTP not configured")
		return
	}
	if !smtpmail.ValidRecipient(to) { // before any dial
		httpx.Detail(w, http.StatusBadRequest, "invalid recipient address")
		return
	}
	mailer := smtpMailerFor(h.d)
	if mailer == nil {
		h.fail(w, "smtp test", errNoMailer)
		return
	}
	pw := cfg.Password.Reveal()
	msg, _ := smtpmail.Compose(smtpmail.Message{FromEmail: cfg.Email, FromName: cfg.FromName, To: strings.TrimSpace(to),
		Subject: "OpenGTM — SMTP Test", HTML: "<p>Your SMTP configuration is working.</p>"})
	ctx, cancel := context.WithTimeout(r.Context(), smtpTestTimeout)
	defer cancel()
	_, err := mailer.Send(ctx, outreachsend.MailConfig{Host: cfg.Host, Port: cfg.Port, Email: cfg.Email, Password: pw, UseTLS: cfg.UseTLS},
		cfg.Email, strings.TrimSpace(to), msg)
	if err != nil {
		httpx.Detail(w, http.StatusBadRequest, smtpErrText(err, pw))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "Test email sent to " + to})
}

type smtpErr string

func (e smtpErr) Error() string { return string(e) }

const errNoMailer = smtpErr("smtp mailer unavailable")
