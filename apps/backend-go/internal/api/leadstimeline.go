package api

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadtimeline"
)

// registerTimeline registers GET /api/lead/{lead_id}/timeline (current_workspace only: viewers may read).
func registerTimeline(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := leadReadRoutes{d: d}
	r.Handle("GET /api/lead/{lead_id}/timeline", http.HandlerFunc(h.timeline),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity))
}

var (
	queryFloatRE  = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	queryNonFinRE = regexp.MustCompile(`(?i)^[+-]?(inf|infinity|nan)$`)
)

// optFloat is an `Optional[float]` query parameter (pydantic lax mode: whitespace trimmed, single underscores
// between digits tolerated, inf/infinity/nan accepted; FastAPI query floats allow them).
func (v *queryValidator) optFloat(name string) *float64 {
	raw, present := v.last(name)
	if !present {
		return nil
	}
	s := strings.TrimSpace(raw)
	bad := s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__")
	if !bad {
		s = strings.ReplaceAll(s, "_", "")
		var f float64
		switch {
		case queryNonFinRE.MatchString(s):
			switch strings.ToLower(strings.TrimLeft(s, "+-")) {
			case "nan":
				f = math.NaN()
			default:
				f = math.Inf(1)
				if s[0] == '-' {
					f = math.Inf(-1)
				}
			}
			return &f
		case queryFloatRE.MatchString(s):
			f, _ = strconv.ParseFloat(s, 64) // overflow yields ±Inf like Rust's parse
			return &f
		}
	}
	v.errs = append(v.errs, httpx.FieldError{Type: "float_parsing", Loc: []any{"query", name}, Input: raw,
		Msg: "Input should be a valid number, unable to parse string as a number"})
	return nil
}

func (h leadReadRoutes) timeline(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	limit := v.integer("limit", 100, i64(1), i64(200))
	before := v.optFloat("before")
	beforeID, _ := v.text("before_id", 255)
	id, none, ok := pathLeadID(w, r, v.errs)
	if !ok {
		return
	}
	if none {
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
		return
	}
	wc, _ := identity.WorkspaceFrom(r.Context())
	var page leadtimeline.Page
	var lead *leadsread.Lead
	if !h.inTx(w, r, "timeline", func(tx pgx.Tx) (err error) {
		if lead, err = oneLead(r, tx, id); err != nil || lead == nil {
			return err
		}
		page, err = leadtimeline.Build(r.Context(), tx, wc.WorkspaceID, *lead, int(limit), before, beforeID)
		return err
	}) {
		return
	}
	if lead == nil {
		httpx.Detail(w, http.StatusNotFound, leadNotFound)
		return
	}
	writePy(w, page)
}
