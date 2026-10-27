package warehouse

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

const (
	requestTimeout = 30 * time.Second
	maxBody        = 1 << 20 // cap on every response body read
	errTextChars   = 300
)

// SecretGetter resolves a workspace secret; an empty value means unset.
type SecretGetter func(ctx context.Context, wsID, name string) (string, error)

// Result is WarehouseBatchResult.
type Result struct {
	Success    bool
	Summary    string
	Error      string
	ExternalID string
}

// Client sends the export over the SSRF-guarded egress dialer. Destinations are
// operator-supplied URLs, so https is mandatory and redirects are never followed.
type Client struct {
	http     *http.Client
	appEnv   string
	allowSet []string
	outbound secrets.OutboundPolicy
}

// WithOutbound sets the policy for header_secret_ref (R2). The zero policy is the default prefixes
// with no registry deny-list, so a client that is never configured still refuses CRM/provider names.
func (c Client) WithOutbound(p secrets.OutboundPolicy) Client {
	c.outbound = p
	return c
}

// NewClient builds a client whose every connection is vetted by egress.NewDialContext.
// allowHosts is honoured only when appEnv is test or bench; production passes none.
func NewClient(appEnv string, allowHosts []string) Client { return newClient(appEnv, allowHosts, nil) }

// newClient is NewClient with a root-CA override, the test hook for local TLS servers.
func newClient(appEnv string, allowHosts []string, roots *x509.CertPool) Client {
	tr := &http.Transport{
		Proxy:               nil, // a proxy would bypass the dial-time vetting
		DialContext:         egress.NewDialContext(appEnv, allowHosts),
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	return Client{
		appEnv:   appEnv,
		allowSet: allowHosts,
		http: &http.Client{
			Transport:     tr,
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func cfgText(cfg map[string]any, key string) string {
	if v := cfg[key]; pyTruthy(v) {
		return rowtmpl.PyStr(v)
	}
	return ""
}

// Sync POSTs the export (manifest line then records, chunked) to config["url"]. Transport and
// URL-safety failures are errors (Python raises); a missing secret or a non-2xx response is a
// failed Result.
func Sync(ctx context.Context, c Client, wsID string, dest DestRef, b *Buffer, get SecretGetter) (Result, error) {
	rawURL, ok := dest.Config["url"]
	if !ok {
		return Result{}, errors.New(`missing destination config "url"`)
	}
	target := rowtmpl.PyStr(rawURL)
	if err := vetURL(c, target); err != nil {
		return Result{}, err
	}
	ref := cfgText(dest.Config, "header_secret_ref")
	secret := ""
	if ref != "" && secrets.CheckOutboundRef(ref, c.outbound) != nil {
		// A stored ref outside the outbound namespace reads as missing: nothing is looked up or sent (S-M2).
		return Result{Error: "warehouse authentication secret is missing"}, nil
	}
	if ref != "" {
		if get == nil {
			return Result{}, errors.New("workspace secret lookup failed")
		}
		v, err := get(ctx, wsID, ref)
		if err != nil {
			// The getter's text is never propagated: it must not be able to carry a secret.
			return Result{}, errors.New("workspace secret lookup failed")
		}
		secret = v
	}
	if secret == "" {
		return Result{Error: "warehouse authentication secret is missing"}, nil
	}
	headerName := cfgText(dest.Config, "header_name")
	if headerName == "" {
		headerName = "Authorization"
	}
	hc := c.http
	if hc == nil {
		hc = NewClient("production", nil).http
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, io.NopCloser(b.Reader()))
	if err != nil {
		return Result{}, errors.New("warehouse request could not be built")
	}
	req.ContentLength = -1 // streamed: chunked transfer encoding
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Idempotency-Key", "warehouse:"+dest.ID+":"+b.runID)
	req.Header.Set(headerName, secret)

	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, sendError(err, secret)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil && len(body) == 0 && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		body = nil // the upload was accepted; a broken body only costs the external id
	}
	res := classify(resp.StatusCode, resp.Header, body, secretForms(secret)...)
	if res.Success {
		m := b.Manifest()
		res.Summary = fmt.Sprintf("exported %d rows; sha256=%s", b.RowCount(), m["records_sha256"].(string)[:12])
	}
	return res, nil
}

// vetURL refuses anything but a credential-free https URL to a public host, before any secret
// is resolved. The dial-time guard re-checks every resolved address.
func vetURL(c Client, target string) error {
	if u, err := url.Parse(strings.TrimSpace(target)); err == nil && u.User != nil {
		return errors.New("warehouse url must not contain credentials")
	}
	if err := egress.CheckURLFor(c.appEnv, c.allowSet, target, false); err != nil {
		return err
	}
	return nil
}

// sendError reports a transport failure without the URL (it may carry a token) or the secret.
func sendError(err error, secret string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := strings.ReplaceAll(err.Error(), secret, "[redacted]")
	if errors.Is(err, context.DeadlineExceeded) {
		msg = "request timed out"
	}
	return fmt.Errorf("warehouse request failed: %s", msg)
}

// classify turns the response into the shared outcome (Summary is filled by Sync).
func classify(status int, hdr http.Header, body []byte, secretValues ...string) Result {
	if status < 200 || status > 299 {
		// The receiver's body is never echoed: a hostile receiver can return the credential in an
		// encoding no redaction list anticipates (URL-encoded, base64, split) (S-L27).
		return Result{Error: fmt.Sprintf("warehouse HTTP %d", status)}
	}
	id := hdr.Get("x-job-id")
	if id == "" {
		id = hdr.Get("location")
	}
	if id == "" {
		id = bodyJobID(body)
	}
	return Result{Success: true, ExternalID: redact(id, secretValues)}
}

// secretForms is the secret plus the spellings a receiver may echo: the value after a leading auth scheme word
// ("Bearer x" echoed as "x") and the JSON-escaped form.
func secretForms(secret string) []string {
	forms := []string{secret}
	if _, rest, ok := strings.Cut(secret, " "); ok && len(rest) >= 8 {
		forms = append(forms, rest)
	}
	for _, f := range append([]string(nil), forms...) {
		if b, err := json.Marshal(f); err == nil {
			if esc := string(b[1 : len(b)-1]); esc != f {
				forms = append(forms, esc)
			}
		}
	}
	return forms
}

// redact replaces every known secret value, and a trailing partial one left by the body cap.
func redact(text string, secrets []string) string {
	for _, sec := range secrets {
		if sec == "" {
			continue
		}
		text = strings.ReplaceAll(text, sec, "[redacted]")
		for n := len(sec) - 1; n >= 8; n-- {
			if strings.HasSuffix(text, sec[:n]) {
				text = strings.TrimSuffix(text, sec[:n]) + "[redacted]"
				break
			}
		}
	}
	return text
}

// bodyJobID is str(parsed.get("job_id") or parsed.get("id") or "") for a JSON object body;
// anything else yields "".
func bodyJobID(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return ""
	}
	if _, err := dec.Token(); err != io.EOF {
		return ""
	}
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	val := m["job_id"]
	if !pyTruthy(val) {
		val = m["id"]
	}
	if !pyTruthy(val) {
		return ""
	}
	return rowtmpl.PyStr(pyValue(val))
}

// pyValue converts decoded JSON numbers to the Go types PyStr renders like Python's.
func pyValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		if !strings.ContainsAny(string(x), ".eE") {
			if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
				return n
			}
			return x // beyond int64: the digits are already Python's str(int)
		}
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = pyValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = pyValue(e)
		}
		return out
	}
	return v
}
