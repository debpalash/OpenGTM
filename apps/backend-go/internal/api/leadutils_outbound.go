package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo" // registers tech_stack
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	verifyEmailTimeout = 60 * time.Second // outer bound; the SMTP probe has its own budget
	techStackTimeout   = 30 * time.Second // one homepage (plus robots.txt and at most a few redirect hops)
	// techStackMaxInFlight bounds concurrent website fetches process-wide: the per-workspace limit is
	// 30/minute, but many workspaces together must not pin an unbounded number of sockets.
	techStackMaxInFlight = 16
)

var techStackSlots = make(chan struct{}, techStackMaxInFlight)

// resolver reads settings and credentials through the secrets envelope; nil without an envelope or pool.
func (h *leadUtils) resolver() *secrets.Resolver {
	if h.d.Envelope == nil || h.d.Pool == nil {
		return nil
	}
	return secrets.NewResolver(h.d.Envelope, secrets.NewPgStore(h.d.Pool), h.d.getenv())
}

// globalSetting resolves an operator setting (app_settings, then env), never a workspace one: a tenant must not
// be able to switch on website fetching or aim Reacher at an internal host.
func (h *leadUtils) globalSetting(ctx context.Context, key string) (string, error) {
	if res := h.resolver(); res != nil {
		v, _, err := res.Get(ctx, "", key)
		return v, err
	}
	return h.d.getenv()(key), nil
}

func (h *leadUtils) allowHosts() []string {
	allow := append([]string(nil), h.d.EgressAllowHosts...)
	for _, a := range strings.Split(h.d.getenv()("EGRESS_ALLOW_HOSTS"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			allow = append(allow, a)
		}
	}
	return allow
}

// verifyDeps is the workspace-aware cascade wiring (the same as the MCP server's): system DNS, SMTP over the
// egress dial guard, the Postgres per-workspace cache and Reacher first when the operator configured it. Reacher
// URL and timeout are operator settings; every Reacher call goes through the operator egress client.
func (h *leadUtils) verifyDeps() emailverify.Deps {
	if leadUtilsVerifyDeps != nil {
		return leadUtilsVerifyDeps(h.d)
	}
	h.vOnce.Do(func() {
		res := h.resolver()
		allow := h.allowHosts()
		cfg := emailverify.LiveConfig{
			Pool:         h.d.Pool,
			AppEnv:       h.d.Config.AppEnv,
			EgressAllow:  allow,
			HTTP:         egress.NewOperatorClient(allow),
			SMTPDisabled: h.d.getenv()("LEADGEN_SMTP_VERIFY") == "0",
			HeloHost:     h.d.getenv()("LEADGEN_SMTP_HELO"),
			MailFrom:     h.d.getenv()("LEADGEN_SMTP_MAIL_FROM"),
			Keys: func(ctx context.Context, ws, envVar string) (string, error) {
				if res == nil {
					return "", nil
				}
				v, _, err := res.Get(ctx, ws, envVar)
				return v, err
			},
			ReacherSettings: func(ctx context.Context, ws string) map[string]any {
				out := map[string]any{}
				if res == nil {
					return out
				}
				for _, k := range []string{"REACHER_URL", "REACHER_TIMEOUT"} {
					v, _, err := res.Get(ctx, "", k)
					if err != nil {
						return map[string]any{} // fail closed: Reacher stays off
					}
					if v != "" {
						out[k] = v
					}
				}
				v, _, err := res.Get(ctx, ws, "REACHER_ENABLED")
				if err != nil {
					return map[string]any{}
				}
				if v != "" {
					out["REACHER_ENABLED"] = v
				}
				return out
			},
		}
		if h.d.PyWorker != nil { // never wrap a nil *Client in a non-nil interface
			cfg.Py = h.d.PyWorker
		}
		h.vd = emailverify.NewLive(cfg)
	})
	return h.vd
}

// verifyEmail is POST /api/leads/verify-email: one address through the shared verification cascade, scoped to the
// authenticated workspace (never one named in the body).
func (h *leadUtils) verifyEmail(w http.ResponseWriter, r *http.Request, raw string) {
	email := providers.PyStrip(raw)
	if !strings.Contains(email, "@") || strings.Contains(email, " ") {
		httpx.Detail(w, http.StatusBadRequest, "email must be a single address")
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), verifyEmailTimeout)
	defer cancel()
	vr, err := emailverify.Cascade(ctx, h.verifyDeps(), wc.WorkspaceID, email)
	if err != nil {
		h.d.logger().Error("leadutils: verify-email failed", "err_type", errTypeName(err))
		serverError(w)
		return
	}
	out := wbcrud.NewObj()
	out.Set("email", email)
	out.Set("status", vr.Status)
	out.Set("valid", vr.Status == emailverify.StatusValid)
	out.Set("deliverable", vr.Deliverable())
	out.Set("confidence", vr.Confidence)
	out.Set("source", vr.Source)
	out.Set("detail", vr.Detail)
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *leadUtils) techClient() *http.Client {
	if h.d.Egress != nil {
		return h.d.Egress
	}
	return egress.NewClientForEnv(h.d.Config.AppEnv, h.d.EgressAllowHosts)
}

// techStack is POST /api/leads/tech-stack: the built-in tech_stack provider on the caller's domain. The fetch is
// off unless the operator set TECH_STACK_WEBSITE_FETCH_ENABLED (409 otherwise, no network call is made). Every
// hop passes the egress guard inside the provider: private, loopback and metadata hosts are refused (also
// after a redirect), the body is capped and the whole call is time-bounded.
func (h *leadUtils) techStack(w http.ResponseWriter, r *http.Request, raw string) {
	domain := providers.PyStrip(raw)
	if domain == "" {
		httpx.Detail(w, http.StatusBadRequest, "domain is required")
		return
	}
	website := domain
	if !strings.HasPrefix(domain, "http://") && !strings.HasPrefix(domain, "https://") {
		website = "https://" + domain
	}
	p, ok := providers.Get("tech_stack")
	if !ok {
		h.d.logger().Error("leadutils: tech_stack provider is not registered")
		serverError(w)
		return
	}
	settings := map[string]any{}
	for _, k := range []string{"TECH_STACK_WEBSITE_FETCH_ENABLED", "TECH_STACK_RESPECT_ROBOTS"} {
		v, err := h.globalSetting(r.Context(), k)
		if err != nil {
			h.d.logger().Error("leadutils: tech-stack setting lookup failed", "err_type", errTypeName(err))
			serverError(w)
			return
		}
		if v != "" {
			settings[k] = v
		}
	}
	select {
	case techStackSlots <- struct{}{}:
		defer func() { <-techStackSlots }()
	default:
		httpx.Detail(w, http.StatusServiceUnavailable, "Too many website fetches in progress; retry shortly")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), techStackTimeout)
	defer cancel()
	out, err := p.Enrich(ctx, h.techClient(), func() (string, error) { return "", nil }, providers.Input{
		Lead:     map[string]any{"company": domain, "website": website, "id": 0},
		Settings: settings,
	})
	if err != nil {
		h.d.logger().Error("leadutils: tech-stack failed", "err_type", errTypeName(err))
		serverError(w)
		return
	}
	if out.Error == "website_fetch_disabled" {
		httpx.Detail(w, http.StatusConflict, "website fetches are disabled; set TECH_STACK_WEBSITE_FETCH_ENABLED=1")
		return
	}
	fields := out.Extra
	if fields == nil {
		fields = providers.NewObj()
	}
	body := wbcrud.NewObj()
	body.Set("domain", domain)
	body.Set("success", out.Error == "")
	tech, _ := fields.Get("technologies")
	body.Set("technologies", tech)
	body.Set("fields", fields)
	if out.Error == "" {
		body.Set("error", nil)
	} else {
		body.Set("error", out.Error)
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}
