package api

import (
	"net/http"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// wsCloseForbidden is the ASGI close code FastAPI sends before accept; Starlette turns it into an
// HTTP 403 handshake refusal.
const wsCloseForbidden = 4403

var (
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 10 * time.Second
)

func handshakeStatusForClose(code int) int { _ = code; return http.StatusOK }

// registerWorkbookWS registers GET .../ws WebSocket (Task 29). Scaffold stub: the owning task fills the body.
func registerWorkbookWS(r *httpx.Router, d Deps) {
	_, _ = r, d
}
