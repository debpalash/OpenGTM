package httpx

import "net/http"

// Header is an extra response header attached to an error.
type Header struct{ Key, Value string }

// FieldError is one pydantic-style validation error entry.
type FieldError struct {
	Type  string `json:"type"`
	Loc   []any  `json:"loc"`
	Msg   string `json:"msg"`
	Input any    `json:"input"`
	// Ctx carries the constraint (e.g. {"ge": 1}); omitted when nil, as pydantic does.
	Ctx map[string]any `json:"ctx,omitempty"`
}

// Detail writes FastAPI's HTTPException body {"detail": detail}.
func Detail(w http.ResponseWriter, status int, detail string, hdr ...Header) {
	for _, h := range hdr {
		w.Header().Set(h.Key, h.Value)
	}
	WriteJSON(w, status, map[string]any{"detail": detail})
}

// ValidationError writes FastAPI's 422 body.
func ValidationError(w http.ResponseWriter, errs ...FieldError) {
	if errs == nil {
		errs = []FieldError{}
	}
	for i := range errs {
		if errs[i].Loc == nil {
			errs[i].Loc = []any{}
		}
	}
	WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": errs})
}
