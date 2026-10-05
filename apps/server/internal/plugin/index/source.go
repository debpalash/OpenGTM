package index

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Download limits. A bundle never legitimately exceeds the installer's own
// total-size limit; the index and its signature are small text documents.
const (
	MaxIndexBytes     = 16 << 20
	maxSignatureBytes = 64 << 10
	MaxBundleBytes    = 80 << 20
)

// IndexFile is the file name looked up in a directory or URL ending in "/".
const IndexFile = "index.json"

// ErrNotFound is returned by fetches for a document that does not exist
// (HTTP 404 or a missing file), so callers can treat an absent signature as
// "unsigned" while still failing on every other error.
var ErrNotFound = errors.New("not found")

// Source locates an index and fetches documents next to it.
type Source struct {
	// Location is the index document: a local file, or an http(s) URL.
	Location  string
	remote    *url.URL // set for http(s) sources
	dir       string   // directory of a local index
	client    *http.Client
	userAgent string
}

// NewHTTPClient builds the client used for index and bundle downloads. It
// follows at most five redirects and refuses to leave https.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if prev := via[len(via)-1]; prev.URL.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("refusing a redirect from https to http")
			}
			return checkRemote(req.URL)
		},
	}
}

// OpenSource interprets loc: an http(s) URL, a file:// URL, a path to an
// index file, or a directory holding index.json.
func OpenSource(loc string, client *http.Client, userAgent string) (*Source, error) {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return nil, errors.New("no plugin index configured (set OPENGTM_PLUGIN_INDEX, plugins.index_url or --index)")
	}
	if client == nil {
		client = NewHTTPClient()
	}
	s := &Source{client: client, userAgent: userAgent}
	switch {
	case strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://"):
		u, err := url.Parse(loc)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("plugin index URL %q is invalid", loc)
		}
		if u.User != nil {
			return nil, errors.New("plugin index URL must not contain credentials")
		}
		if err := checkRemote(u); err != nil {
			return nil, err
		}
		u.Fragment = ""
		if strings.HasSuffix(u.Path, "/") || u.Path == "" {
			u.Path = path.Join("/"+u.Path, IndexFile)
		}
		s.remote, s.Location = u, u.String()
	default:
		p := strings.TrimPrefix(loc, "file://")
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			p = filepath.Join(p, IndexFile)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		s.Location, s.dir = abs, filepath.Dir(abs)
	}
	return s, nil
}

// Remote reports whether the index is served over HTTP(S).
func (s *Source) Remote() bool { return s.remote != nil }

// checkRemote allows https, and plain http only to a loopback host (local
// mirrors and tests). Integrity never depends on the transport, but there is
// no reason to leak what an operator installs or to let a network attacker
// serve a stale index.
func checkRemote(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return nil
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("refusing plain http to %s: use https (http is allowed for loopback only)", h)
	}
	return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
}

// FetchIndex returns the raw index bytes and its detached signature, if any.
func (s *Source) FetchIndex(ctx context.Context) (doc, sig []byte, err error) {
	doc, err = s.get(ctx, s.Location, MaxIndexBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch plugin index %s: %w", s.Location, err)
	}
	sig, err = s.get(ctx, s.Location+".sig", maxSignatureBytes)
	if errors.Is(err, ErrNotFound) {
		return doc, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("fetch plugin index signature: %w", err)
	}
	return doc, sig, nil
}

// get reads a small document, bounded by max.
func (s *Source) get(ctx context.Context, loc string, max int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := s.copyTo(ctx, loc, &buf, max); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Resolve turns a version's url into a fetchable location, enforcing that a
// remote index cannot point at local files and that nothing escapes a local
// mirror directory.
func (s *Source) Resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("empty bundle url")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("bundle url %q: %w", ref, err)
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		if u.User != nil {
			return "", errors.New("bundle url must not contain credentials")
		}
		if err := checkRemote(u); err != nil {
			return "", err
		}
		return u.String(), nil
	}
	if u.Scheme != "" || u.Host != "" {
		return "", fmt.Errorf("bundle url %q: only https URLs and paths relative to the index are allowed", ref)
	}
	if s.remote != nil {
		r := s.remote.ResolveReference(u)
		if err := checkRemote(r); err != nil {
			return "", err
		}
		return r.String(), nil
	}
	rel := filepath.FromSlash(u.Path)
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("bundle url %q escapes the index directory", ref)
	}
	return filepath.Join(s.dir, rel), nil
}

// FetchBundle streams a resolved location into w, failing if it exceeds max.
func (s *Source) FetchBundle(ctx context.Context, loc string, w io.Writer, max int64) error {
	return s.copyTo(ctx, loc, w, max)
}

func (s *Source) copyTo(ctx context.Context, loc string, w io.Writer, max int64) error {
	if !strings.HasPrefix(loc, "http://") && !strings.HasPrefix(loc, "https://") {
		f, err := os.Open(loc)
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		defer f.Close()
		return copyLimited(w, f, max)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return err
	}
	if s.userAgent != "" {
		req.Header.Set("User-Agent", s.userAgent)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, redactURL(loc))
	}
	return copyLimited(w, resp.Body, max)
}

func copyLimited(w io.Writer, r io.Reader, max int64) error {
	n, err := io.Copy(w, io.LimitReader(r, max+1))
	if err != nil {
		return err
	}
	if n > max {
		return fmt.Errorf("download exceeds the %d byte limit", max)
	}
	return nil
}

// redactURL drops the query (which may carry tokens) from error messages.
func redactURL(s string) string {
	if u, err := url.Parse(s); err == nil {
		u.RawQuery, u.User = "", nil
		return u.String()
	}
	return s
}
