package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
)

// eventsHandler streams the caller's workspace progress as server-sent
// events. Authorization goes through FastAPI (token from the Authorization
// header or ?token=, workspace from X-Workspace-Id or ?workspace_id=, since
// EventSource cannot set headers). Events are unnamed "data:" messages so
// EventSource.onmessage receives them, as with the legacy /api/events stream.
func eventsHandler(d Deps) http.Handler {
	stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.Hub == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"detail": "Live events unavailable"})
			return
		}
		ws, _ := authz.FromContext(r.Context())
		rc := http.NewResponseController(w)
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		sub := d.Hub.Subscribe(ws.WorkspaceID, 256)
		defer sub.Close()
		// Tell the client how long to wait before reconnecting, and flush
		// headers now so proxies and browsers see the stream open.
		fmt.Fprint(w, "retry: 3000\n: connected\n\n")
		if err := rc.Flush(); err != nil {
			return
		}
		beat := time.NewTicker(d.Heartbeat)
		defer beat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-d.Closing:
				return
			case <-beat.C:
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
			case e, ok := <-sub.C():
				if !ok {
					return
				}
				data, err := json.Marshal(e)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
					return
				}
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	})
	if d.Authz == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz.WriteError(w, authz.ErrUnavailable)
		})
	}
	return d.Authz.Middleware(stream)
}
