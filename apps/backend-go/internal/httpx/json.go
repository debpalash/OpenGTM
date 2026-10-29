// Package httpx is the HTTP foundation for the Go API: a method-aware router
// with FastAPI-compatible error bodies, plus request id, CORS and gzip
// middleware that mirror the Python app's behaviour.
package httpx

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as compact JSON (no HTML escaping, like Starlette's
// JSONResponse) with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, `{"detail":"Internal Server Error"}`, http.StatusInternalServerError)
		return
	}
	body := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
