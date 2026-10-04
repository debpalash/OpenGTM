package server

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

func osDirFS(dir string) fs.FS { return os.DirFS(dir) }

// spa serves the built dashboard: real files when they exist, index.html for
// client-side routes. Vite fingerprints everything under /assets/, so those
// are immutable; index.html must revalidate so a deploy takes effect at once.
type spa struct {
	root fs.FS
	log  *slog.Logger

	mu  sync.Mutex
	gzc map[string]gzEntry
}

type gzEntry struct {
	size    int64
	modTime time.Time
	data    []byte
}

func newSPA(root fs.FS, log *slog.Logger) http.Handler {
	return &spa{root: root, log: log, gzc: map[string]gzEntry{}}
}

func (s *spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "index.html" {
		s.serveIndex(w, r)
		return
	}
	if hidden(name) {
		s.serveIndex(w, r)
		return
	}
	f, err := s.root.Open(name)
	if err == nil {
		defer f.Close()
		if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			}
			s.serveFile(w, r, name, f, st)
			return
		}
	}
	if strings.HasPrefix(name, "assets/") {
		// A missing fingerprinted asset is a stale client; answering with
		// index.html would hand it HTML in place of JavaScript or CSS.
		http.NotFound(w, r)
		return
	}
	s.serveIndex(w, r)
}

// hidden rejects dotfiles (e.g. the embed placeholder) anywhere in the path.
func hidden(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func (s *spa) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := s.root.Open("index.html")
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "The web UI is not built into this binary. Set OPENGTM_WEB_DIR to the "+
			"built apps/web/dist directory, or embed it (see internal/server/webdist).", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	s.serveFile(w, r, "index.html", f, st)
}

var compressible = map[string]bool{
	".html": true, ".js": true, ".mjs": true, ".css": true, ".json": true,
	".svg": true, ".txt": true, ".xml": true, ".map": true, ".webmanifest": true,
}

// serveFile serves f, gzip-encoded (as nginx did) when the client accepts it
// and the type benefits. Compressed bodies are cached by path, size and
// modification time; the embedded build never changes and a web dir rarely.
func (s *spa) serveFile(w http.ResponseWriter, r *http.Request, name string, f fs.File, st fs.FileInfo) {
	ext := path.Ext(name)
	if ctype := mime.TypeByExtension(ext); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.Header().Add("Vary", "Accept-Encoding")
	if compressible[ext] && st.Size() >= 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		gz, err := s.gzipped(name, f, st)
		if err == nil {
			w.Header().Set("Content-Encoding", "gzip")
			http.ServeContent(w, r, name, st.ModTime(), bytes.NewReader(gz))
			return
		}
		s.log.Warn("gzip failed; serving identity", "file", name, "err", err)
		seeker, ok := f.(io.Seeker)
		if !ok {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		seeker.Seek(0, io.SeekStart)
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		rs = bytes.NewReader(data)
	}
	http.ServeContent(w, r, name, st.ModTime(), rs)
}

func (s *spa) gzipped(name string, f fs.File, st fs.FileInfo) ([]byte, error) {
	s.mu.Lock()
	e, ok := s.gzc[name]
	s.mu.Unlock()
	if ok && e.size == st.Size() && e.modTime.Equal(st.ModTime()) {
		return e.data, nil
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := io.Copy(zw, f); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() == 0 {
		return nil, errors.New("empty gzip output")
	}
	s.mu.Lock()
	s.gzc[name] = gzEntry{size: st.Size(), modTime: st.ModTime(), data: buf.Bytes()}
	s.mu.Unlock()
	return buf.Bytes(), nil
}
