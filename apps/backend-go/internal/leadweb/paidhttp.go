package leadweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

type providerRequest struct {
	endpoint string
	key      config.Secret
	body     any
	accept   string
	timeout  time.Duration
	maxBytes int64
}

var errProviderResponse = errors.New("web provider request failed")

// postProvider keeps the guarded transport, never follows redirects (including
// 307/308 body replays), and bounds the complete decompressed response. No URL,
// query, response body, or key is included in returned errors or logs.
func postProvider(ctx context.Context, hc *http.Client, p providerRequest) (http.Header, []byte, error) {
	if hc == nil || p.key == "" {
		return nil, nil, errProviderResponse
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(p.body)
	if err != nil {
		return nil, nil, errProviderResponse
	}
	timeout := p.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, errProviderResponse
	}
	req.Header.Set("Authorization", "Bearer "+p.key.Reveal())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", p.accept)
	req.Header.Set("User-Agent", "OpenGTM")
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errProviderResponse
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, errProviderResponse
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBytes+1))
	if err != nil || int64(len(raw)) > p.maxBytes {
		return nil, nil, errProviderResponse
	}
	return resp.Header, raw, nil
}

// publicPageURL is a strict, DNS-free check. Search hits are not fetched here;
// actual reads must also enforce DNS and redirect policy at the fetch service.
func publicPageURL(raw string) bool {
	if len(raw) > 8192 || strings.ContainsRune(raw, '\\') || strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && u.Opaque == "" && egress.CheckURL(raw, true) == nil
}
