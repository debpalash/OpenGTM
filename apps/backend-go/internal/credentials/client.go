// Package credentials fetches provider credentials from the Python API's
// internal endpoint (GET /internal/v1/credentials/{env_var}).
//
// Contract (matches the Python declarative resolver): the endpoint resolves
// workspace secret -> settings DB -> environment; an unset var is a 200 with an
// empty value. Get returns ("", nil) for empty (treated as "missing" by the
// caller) and an error for transport/5xx/auth failures. Errors never contain
// the service token or any credential value. Only non-empty values are cached
// (60s, per workspace); empties and errors are never cached.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// TTL is how long a non-empty credential stays cached.
	TTL = 60 * time.Second
	// requestTimeout bounds one call to the private-network internal API.
	requestTimeout = 5 * time.Second
	maxBody        = 64 << 10
)

// Client is safe for concurrent use.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
	now     func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
}

type cacheKey struct{ workspaceID, envVar string }

type cacheEntry struct {
	value   string
	expires time.Time
}

// New builds a Client. httpClient may be nil, in which case a plain client with
// a 5s timeout is used. Do NOT pass the egress (SSRF-guarded) client: the
// internal API lives on the private network, which that client blocks.
func New(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      httpClient,
		now:     time.Now,
		cache:   make(map[cacheKey]cacheEntry),
	}
}

// Get returns the credential for envVar in workspaceID. ("", nil) means the
// credential is not configured.
func (c *Client) Get(ctx context.Context, workspaceID, envVar string) (string, error) {
	if workspaceID == "" || envVar == "" {
		return "", errors.New("credentials: workspace id and env var are required")
	}
	key := cacheKey{workspaceID, envVar}
	if v, ok := c.cached(key); ok {
		return v, nil
	}
	v, err := c.fetch(ctx, workspaceID, envVar)
	if err != nil {
		return "", err
	}
	if v != "" {
		c.mu.Lock()
		c.cache[key] = cacheEntry{value: v, expires: c.now().Add(TTL)}
		c.mu.Unlock()
	}
	return v, nil
}

func (c *Client) cached(key cacheKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok {
		return "", false
	}
	if !c.now().Before(e.expires) {
		delete(c.cache, key)
		return "", false
	}
	return e.value, true
}

func (c *Client) fetch(ctx context.Context, workspaceID, envVar string) (string, error) {
	u := c.baseURL + "/internal/v1/credentials/" + url.PathEscape(envVar)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", errors.New("credentials: build request failed")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Workspace-Id", workspaceID)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		// err (a *url.Error) embeds the URL only, never headers; still, do not
		// wrap it so nothing request-derived can leak.
		if ctx.Err() != nil {
			return "", fmt.Errorf("credentials: request for %s aborted: %w", envVar, ctx.Err())
		}
		return "", fmt.Errorf("credentials: transport error fetching %s", envVar)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("credentials: %s is not a servable credential (404)", envVar)
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return "", fmt.Errorf("credentials: unexpected status %d fetching %s", resp.StatusCode, envVar)
	}

	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return "", fmt.Errorf("credentials: malformed response fetching %s", envVar)
	}
	return body.Value, nil
}
