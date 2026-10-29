package httpx

import (
	"net/http"
	"strings"
)

const corsMethods = "DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT"

// CORS mirrors Starlette's CORSMiddleware with allow_credentials=False, all
// methods and headers allowed, max_age=600. origins is comma separated; empty
// means "*".
func CORS(origins string) Middleware {
	var list []string
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			list = append(list, o)
		}
	}
	if len(list) == 0 {
		list = []string{"*"}
	}
	all := false
	set := map[string]bool{}
	for _, o := range list {
		if o == "*" {
			all = true
		}
		set[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			allowed := all || set[origin]
			setOrigin := func() {
				if all {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Add("Vary", "Origin")
				}
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h := w.Header()
				h.Set("Access-Control-Allow-Methods", corsMethods)
				h.Set("Access-Control-Max-Age", "600")
				if rh := r.Header.Get("Access-Control-Request-Headers"); rh != "" {
					h.Set("Access-Control-Allow-Headers", rh)
				}
				if !allowed {
					h.Set("Content-Type", "text/plain; charset=utf-8")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte("Disallowed CORS origin"))
					return
				}
				setOrigin()
				h.Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("OK"))
				return
			}
			if allowed {
				setOrigin()
			}
			next.ServeHTTP(w, r)
		})
	}
}
