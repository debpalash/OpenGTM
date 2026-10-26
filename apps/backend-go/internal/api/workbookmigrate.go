package api

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// tuneMigrateService lets tests adjust the migrate service (page size, row cap, lead reader).
var tuneMigrateService = func(*wbcrud.Service) {}

// registerWorkbookMigrate registers POST .../migrate (Task 28). Scaffold stub: the owning task fills the body.
func registerWorkbookMigrate(r *httpx.Router, d Deps) {
	_, _ = r, d
}
