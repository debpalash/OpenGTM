package api

import (
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sysfiles"
)

// RegisterSystem registers /api/system/stats and /api/files/{list,download,delete}.
//
// Hardened mode (default, deviation D1): every route needs a valid token; the file routes
// also need a global admin; paths are resolved inside the data dir (symlinks followed and
// checked), delete is never recursive and errors answer a generic message. FILES_API_MODE=legacy
// reproduces Python exactly (no auth, string-prefix check, rmtree, error text) for diffing only.
func RegisterSystem(r *httpx.Router, d Deps) {
	s := newSysRoutes(d)
	if s.legacy {
		s.log.Warn("FILES_API_MODE=legacy: /api/files/* and /api/system/stats are UNAUTHENTICATED and delete is recursive; use for contract diffing only")
		s.mount(r, nil, nil)
		return
	}
	authn := identity.Authenticate(d.Signer, d.Identity)
	s.mount(r, []httpx.Middleware{authn}, []httpx.Middleware{authn, identity.RequireGlobalAdmin()})
}

type sysRoutes struct {
	fs     *sysfiles.FS
	legacy bool
	dbURL  string
	root   string // project root walked for project_size (parent of the data dir, like ROOT_DIR)
	log    *slog.Logger
}

func newSysRoutes(d Deps) sysRoutes {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	dir := d.DataDir
	if dir == "" {
		dir = "data"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	legacy := d.Flags.FilesAPIMode == config.FilesAPIModeLegacy
	return sysRoutes{
		fs: sysfiles.New(abs, legacy), legacy: legacy, dbURL: d.Config.DatabaseURL,
		root: filepath.Dir(abs), log: log,
	}
}

func (s sysRoutes) mount(r *httpx.Router, statsMW, fileMW []httpx.Middleware) {
	r.Handle("GET /api/system/stats", http.HandlerFunc(s.stats), statsMW...)
	r.Handle("GET /api/files/list", http.HandlerFunc(s.list), fileMW...)
	r.Handle("GET /api/files/download/{file_path...}", http.HandlerFunc(s.download), fileMW...)
	r.Handle("DELETE /api/files/delete/{file_path...}", http.HandlerFunc(s.remove), fileMW...)
}

// fail answers an unexpected error: Python's str(e) in legacy mode, a generic message
// otherwise (the real error goes to the server log only).
func (s sysRoutes) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("system files: "+what+" failed", "err", err)
	if s.legacy {
		httpx.Detail(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.Detail(w, http.StatusInternalServerError, "Internal error")
}

func (s sysRoutes) stats(w http.ResponseWriter, r *http.Request) {
	st, err := sysfiles.Stats(r.Context(), "/", s.root, s.dbURL)
	if err != nil {
		s.fail(w, "stats", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

func (s sysRoutes) list(w http.ResponseWriter, r *http.Request) {
	l, err := s.fs.List(r.URL.Query().Get("path"))
	switch {
	case errors.Is(err, sysfiles.ErrAccessDenied):
		httpx.Detail(w, http.StatusForbidden, "Invalid path: Access denied")
	case err != nil:
		s.fail(w, "list", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, l)
	}
}

func (s sysRoutes) download(w http.ResponseWriter, r *http.Request) {
	p, st, err := s.fs.Open(r.PathValue("file_path"))
	switch {
	case errors.Is(err, sysfiles.ErrAccessDenied):
		httpx.Detail(w, http.StatusForbidden, "Access denied")
		return
	case errors.Is(err, sysfiles.ErrNotFound):
		httpx.Detail(w, http.StatusNotFound, "File not found")
		return
	case err != nil:
		s.fail(w, "download", err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		s.fail(w, "download", err)
		return
	}
	defer f.Close()
	ctype := mime.TypeByExtension(filepath.Ext(p))
	if ctype == "" {
		ctype = "text/plain" // Starlette FileResponse default
		if !s.legacy {
			ctype = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(p)}))
	if !s.legacy {
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s sysRoutes) remove(w http.ResponseWriter, r *http.Request) {
	err := s.fs.Delete(r.PathValue("file_path"))
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "success", "message": "File deleted"})
	case errors.Is(err, sysfiles.ErrAccessDenied):
		httpx.Detail(w, http.StatusForbidden, "Invalid path: Access denied")
	case errors.Is(err, sysfiles.ErrNotFound):
		if s.legacy {
			// Python raises HTTPException(404) inside its own try/except Exception -> 500 "404: ...".
			httpx.Detail(w, http.StatusInternalServerError, "404: File not found or could not be deleted")
			return
		}
		httpx.Detail(w, http.StatusNotFound, "File not found or could not be deleted")
	case errors.Is(err, sysfiles.ErrNotEmpty):
		httpx.Detail(w, http.StatusBadRequest, "Directory is not empty")
	case errors.Is(err, sysfiles.ErrRoot):
		httpx.Detail(w, http.StatusBadRequest, "Cannot delete the data directory")
	default:
		s.fail(w, "delete", err)
	}
}
