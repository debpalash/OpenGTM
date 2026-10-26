package adsdrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

const (
	requestTimeout = 30 * time.Second
	maxBody        = 1 << 20 // 1 MiB cap on every response body
)

// ErrResponseTooLarge is returned when a platform response exceeds the body cap.
var ErrResponseTooLarge = errors.New("platform response too large")

// Endpoints are the platform base URLs. Production values are ProductionEndpoints; tests
// substitute their servers only through this struct.
type Endpoints struct{ Meta, LinkedIn, Google string }

// ProductionEndpoints are the real platform hosts.
var ProductionEndpoints = Endpoints{
	Meta:     "https://graph.facebook.com",
	LinkedIn: "https://api.linkedin.com",
	Google:   "https://googleads.googleapis.com",
}

// SecretGetter resolves a workspace secret; an empty value means unset.
type SecretGetter func(ctx context.Context, wsID, name string) (string, error)

// Result is AdBatchResult: one outcome shared by every delivery of the batch.
type Result struct {
	Success    bool
	Summary    string
	Error      string
	ExternalID string
}

// Client sends platform requests over the SSRF-guarded egress dialer.
type Client struct {
	Endpoints Endpoints
	Secrets   SecretGetter
	HTTP      *http.Client
}

// NewClient builds a client whose every connection is vetted by egress.NewDialContext.
// allowHosts is honoured only when appEnv is test or bench; production passes none. Redirects
// are never followed and no proxy is used.
func NewClient(secrets SecretGetter, appEnv string, allowHosts []string) Client {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         egress.NewDialContext(appEnv, allowHosts),
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	return Client{
		Endpoints: ProductionEndpoints,
		Secrets:   secrets,
		HTTP: &http.Client{
			Transport:     tr,
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c Client) secret(ctx context.Context, wsID, name string) (string, error) {
	if c.Secrets == nil {
		return "", fmt.Errorf("missing workspace secret %s", name)
	}
	v, err := c.Secrets(ctx, wsID, name)
	if err != nil {
		// The getter's text is never propagated: it must not be able to carry a secret.
		return "", fmt.Errorf("workspace secret %s lookup failed", name)
	}
	if v == "" {
		return "", fmt.Errorf("missing workspace secret %s", name)
	}
	return v, nil
}

// do performs one request. 401/403 map to the Python text, other non-2xx to the status alone;
// the response body of a failure is never read into an error. A 2xx body is returned capped.
func (c Client) do(ctx context.Context, method, rawURL string, hdr map[string]string, ctype string, body []byte) ([]byte, error) {
	hc := c.HTTP
	if hc == nil {
		hc = NewClient(c.Secrets, "production", nil).HTTP
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("platform request could not be built")
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL
		}
		return nil, fmt.Errorf("platform request failed: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return nil, fmt.Errorf("platform authorization or API approval required (%d)", resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("platform request failed (status %d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, errors.New("platform response could not be read")
	}
	if len(raw) > maxBody {
		return nil, ErrResponseTooLarge
	}
	return raw, nil
}

var errBadJSON = errors.New("invalid JSON response from platform")

func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errBadJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errBadJSON
	}
	return v, nil
}

// cfgStr is str(config[key]); a missing key is Python's KeyError.
func cfgStr(cfg map[string]any, key string) (string, error) {
	v, ok := cfg[key]
	if !ok {
		return "", fmt.Errorf("missing destination config %q", key)
	}
	return rowtmpl.PyStr(v), nil
}

// cfgGet is config.get(key, def) stringified.
func cfgGet(cfg map[string]any, key, def string) string {
	if v, ok := cfg[key]; ok {
		return rowtmpl.PyStr(v)
	}
	return def
}

func seg(s string) string { return url.PathEscape(s) }

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// batchRows builds the per-lead identifier rows. remove accepts the already-hashed values the
// matching add persisted, unchanged (lower-cased) rather than hashing them again.
func batchRows(op string, snapshots []map[string]any) []map[string]string {
	var rows []map[string]string
	for _, s := range snapshots {
		row := HashedIdentifiers(s)
		if op == "remove" {
			for _, k := range []string{"email", "phone"} {
				v := strings.ToLower(strings.TrimSpace(orEmptyStr(s[k])))
				if hex64.MatchString(v) {
					row[k] = v
				}
			}
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	return rows
}

// SyncBatch uploads one bounded batch (op add|remove) and returns one result for all of it.
// Platform and transport failures that Python raises are returned as errors; batch-level
// rejections are a failed Result.
func SyncBatch(ctx context.Context, c Client, wsID, destType string, cfg map[string]any, snapshots []map[string]any, op string) (Result, error) {
	if op != "add" && op != "remove" {
		return Result{}, errors.New("ad sync operation must be add or remove")
	}
	rows := batchRows(op, snapshots)
	if len(rows) == 0 {
		return Result{Error: "no valid email or phone identifiers"}, nil
	}
	past := "added"
	if op == "remove" {
		past = "removed"
	}
	switch destType {
	case "meta_ads":
		return c.syncMeta(ctx, wsID, cfg, rows, op, past)
	case "linkedin_ads":
		return c.syncLinkedIn(ctx, wsID, cfg, rows, op, past)
	case "google_ads":
		return c.syncGoogle(ctx, wsID, cfg, rows, op)
	}
	return Result{Error: fmt.Sprintf("unsupported ad destination '%s'", destType)}, nil
}
