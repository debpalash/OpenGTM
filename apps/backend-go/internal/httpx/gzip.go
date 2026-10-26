package httpx

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
)

// Gzip compresses responses of at least minSize bytes for clients that accept
// gzip. It skips text/event-stream and responses that set Content-Encoding.
func Gzip(minSize int) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				next.ServeHTTP(w, r)
				return
			}
			gw := &gzipWriter{ResponseWriter: w, min: minSize}
			defer gw.finish()
			next.ServeHTTP(gw, r)
		})
	}
}

type gzipWriter struct {
	http.ResponseWriter
	min     int
	status  int
	buf     bytes.Buffer
	decided bool
	zw      *gzip.Writer
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.status == 0 {
		g.status = code
	}
}

func (g *gzipWriter) skippable() bool {
	h := g.Header()
	return h.Get("Content-Encoding") != "" || strings.HasPrefix(h.Get("Content-Type"), "text/event-stream")
}

func (g *gzipWriter) passthrough() {
	g.decided = true
	if g.status == 0 {
		g.status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.status)
	if g.buf.Len() > 0 {
		_, _ = g.ResponseWriter.Write(g.buf.Bytes())
		g.buf.Reset()
	}
}

func (g *gzipWriter) compress() {
	g.decided = true
	if g.status == 0 {
		g.status = http.StatusOK
	}
	h := g.Header()
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	g.ResponseWriter.WriteHeader(g.status)
	g.zw = gzip.NewWriter(g.ResponseWriter)
	_, _ = g.zw.Write(g.buf.Bytes())
	g.buf.Reset()
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.decided {
		if g.skippable() {
			g.passthrough()
		} else {
			g.buf.Write(p)
			if g.buf.Len() >= g.min {
				g.compress()
			}
			return len(p), nil
		}
	}
	if g.zw != nil {
		return g.zw.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

// Flush decides in favour of the plain path for undecided small bodies so
// streaming handlers are not delayed.
func (g *gzipWriter) Flush() {
	if !g.decided {
		if g.skippable() || g.buf.Len() < g.min {
			g.passthrough()
		} else {
			g.compress()
		}
	}
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) finish() {
	if !g.decided {
		g.passthrough()
	}
	if g.zw != nil {
		_ = g.zw.Close()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }
