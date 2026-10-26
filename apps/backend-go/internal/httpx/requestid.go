package httpx

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"regexp"
)

type ctxKey int

const requestIDKey ctxKey = 1

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// RequestIDFrom returns the request id stored by RequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

func newUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// RequestID honours a safe incoming X-Request-Id, else generates a uuid4, and
// echoes it on the response and in the request context.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if !safeRequestID.MatchString(id) {
				id = newUUID4()
			}
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
		})
	}
}
