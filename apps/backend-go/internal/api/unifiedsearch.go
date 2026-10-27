package api

import (
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/unisearch"
)

// unifiedResult is SearchResult.model_dump() plus the "description" the route adds from "snippet" (null stays null).
type unifiedResult struct {
	unisearch.Result
	Description *string `json:"description"`
}

type unifiedAnswer struct {
	Query   string          `json:"query"`
	Total   int             `json:"total"`
	Results []unifiedResult `json:"results"`
}

// unified is routers/leads.py public_unified_search. FastAPI validates `limit: int` first (a 422 does not count
// against the 60/minute budget); an empty q answers the empty shape without searching.
func (h *scrapeRoutes) unified(w http.ResponseWriter, r *http.Request) {
	limit, ok := queryInt(w, r, "limit", 20, nil, nil)
	if !ok {
		return
	}
	last := func(name string) string {
		v := r.URL.Query()[name]
		if len(v) == 0 {
			return ""
		}
		return v[len(v)-1]
	}
	q, sources, sortBy, fileType := last("q"), last("sources"), last("sort"), last("file_type")
	if _, set := r.URL.Query()["sort"]; !set {
		sortBy = "relevance"
	}
	h.limited(unifiedRate, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q == "" {
			writePy(w, unifiedAnswer{Query: "", Total: 0, Results: []unifiedResult{}})
			return
		}
		var names []string
		for _, s := range strings.Split(sources, ",") {
			if s = pyStripSpace(s); s != "" {
				names = append(names, s)
			}
		}
		n := len(names)
		if n == 0 {
			n = 1
		}
		per := limit / n
		if limit < 0 {
			per = -((-limit + n - 1) / n) // floor division, like Python
		}
		if per < 5 {
			per = 5
		}
		if per > unifiedMaxPerSrc {
			per = unifiedMaxPerSrc
		}
		res := h.search.SearchUnified(r.Context(), q, names, per, sortBy, fileType)
		out := unifiedAnswer{Query: q, Total: res.Total, Results: make([]unifiedResult, 0, len(res.Results))}
		for _, x := range res.Results {
			out.Results = append(out.Results, unifiedResult{Result: x, Description: x.Snippet})
		}
		writePy(w, out)
	})).ServeHTTP(w, r)
}

func pyStripSpace(s string) string { return strings.TrimSpace(s) }
