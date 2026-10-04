package server

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// newLegacyProxy forwards to FastAPI with the nginx location semantics:
// original Host, X-Real-IP, appended X-Forwarded-For, X-Forwarded-Proto,
// WebSocket upgrades, no response buffering (SSE, streaming chat) and a
// 300s wait for response headers.
func newLegacyProxy(legacyURL string, log *slog.Logger) (http.Handler, error) {
	target, err := url.Parse(legacyURL)
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("server: invalid legacy API URL %q", legacyURL)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 300 * time.Second
	transport.MaxIdleConnsPerHost = 64
	// FastAPI compresses itself; asking for raw bytes would make the proxy
	// decompress and re-send them uncompressed.
	transport.DisableCompression = true

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// SetURL rewrites Host to the target; nginx kept the client's.
			pr.Out.Host = pr.In.Host
			// $proxy_add_x_forwarded_for appends to the inbound chain.
			if prior := pr.In.Header.Values("X-Forwarded-For"); len(prior) > 0 {
				pr.Out.Header["X-Forwarded-For"] = append([]string(nil), prior...)
			}
			pr.SetXForwarded()
			if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				pr.Out.Header.Set("X-Real-IP", ip)
			}
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			// securityHeaders already set these; avoid duplicates.
			resp.Header.Del("X-Frame-Options")
			resp.Header.Del("X-Content-Type-Options")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("legacy API proxy error", "request_id", RequestID(r.Context()),
				"path", r.URL.Path, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"detail": "Legacy API unavailable"})
		},
	}, nil
}
