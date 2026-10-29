package nlcolumn

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// The LLM authors http_url, http_headers and http_body from a prompt that holds user-controlled text (column
// names, sample values), so an injected instruction can steer them. A generated http column is therefore
// checked before it is returned; one that fails is dropped (Result.Dropped) with a warning, and the user can
// still write the column by hand. Run time keeps its own guards (strict egress client, no secrets callback);
// this makes the generator not depend on that single argument.

var (
	placeholderRe = regexp.MustCompile(`\{[^{}]*\}`)
	secretRefRe   = regexp.MustCompile(`(?i)\{\s*secret\s*:`)
	// authHeaderRe matches header names that carry credentials; a generated column must not smuggle one.
	authHeaderRe = regexp.MustCompile(`(?i)auth|cookie|token|secret|password|passwd|api[-_]?key|credential|signature|session`)
)

// httpColumnProblem returns a user-safe reason the generated http config is unacceptable, or "". The reason never
// repeats the model-supplied text.
func httpColumnProblem(col wbcrud.Column) string {
	url, _ := col.Get("http_url").(string)
	if secretRefRe.MatchString(url) {
		return "the URL references a stored secret"
	}
	// Column placeholders ({name}) are expanded per row at run time, so a row value must not be able to choose
	// the destination: the scheme and authority have to be literal.
	if i := strings.Index(url, "://"); i >= 0 {
		auth := url[i+3:]
		if j := strings.IndexAny(auth, "/?#"); j >= 0 {
			auth = auth[:j]
		}
		if strings.ContainsAny(auth, "{}") {
			return "the URL host is built from row data"
		}
		if strings.Contains(auth, "@") {
			return "the URL contains credentials"
		}
	}
	if strings.ContainsAny(strings.SplitN(url, "://", 2)[0], "{}") {
		return "the URL scheme is built from row data"
	}
	if err := egress.CheckURL(placeholderRe.ReplaceAllString(url, "x"), true); err != nil {
		return "the URL is not an allowed public http(s) address"
	}
	if hv := col.Get("http_headers"); hv != nil {
		if h, ok := hv.(*wbcrud.Obj); ok {
			for _, k := range h.Keys() {
				v, _ := h.Get(k)
				if authHeaderRe.MatchString(k) {
					return "it sets an authentication header"
				}
				if s, _ := v.(string); secretRefRe.MatchString(s) {
					return "a header references a stored secret"
				}
			}
		}
	}
	if b := col.Get("http_body"); b != nil {
		raw, _ := json.Marshal(b)
		if secretRefRe.Match(raw) {
			return "the body references a stored secret"
		}
	}
	return ""
}
