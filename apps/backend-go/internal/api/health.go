package api

import (
	"encoding/json"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// registerHealth registers FastAPI's GET /health and GET /api (main.py). Both are public. They are distinct from
// GET /healthz (cmd/api), the Go liveness probe with its own body.
func registerHealth(r *httpx.Router) {
	r.Handle("GET /health", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, orderedBody{{"status", "healthy"}, {"engine", "OpenGTM"}, {"version", "3.0.0"}})
	}))
	r.Handle("GET /api", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, orderedBody{{"status", "ok"}, {"engine", "OpenGTM"}, {"version", "3.0.0"}})
	}))
}

// orderedBody is a JSON object with a fixed key order (FastAPI keeps dict insertion order).
type orderedBody [][2]string

func (o orderedBody) MarshalJSON() ([]byte, error) {
	b := []byte{'{'}
	for i, kv := range o {
		if i > 0 {
			b = append(b, ',')
		}
		k, _ := json.Marshal(kv[0])
		v, _ := json.Marshal(kv[1])
		b = append(append(append(b, k...), ':'), v...)
	}
	return append(b, '}'), nil
}
