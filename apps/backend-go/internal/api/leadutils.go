package api

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Test seam: nil means the production verifier wiring.
var leadUtilsVerifyDeps func(d Deps) emailverify.Deps

// scoreBody is ScoreLeadBody: lead_id: Optional[int], lead: Optional[dict].
type scoreBody struct {
	LeadID *int64
	Lead   *wbcrud.Obj
}

func parseScoreBody(raw []byte) (scoreBody, []httpx.FieldError) { return scoreBody{}, nil }

// parseStringField validates a body model with one required str field.
func parseStringField(raw []byte, field string) (string, []httpx.FieldError) { return "", nil }

// scoreInline is score_lead(Lead.from_dict(lead)); a wrong-typed field is a *leadscore.PyError.
func scoreInline(lead *wbcrud.Obj) (company any, score int, err error) { return nil, 0, nil }

// registerLeadUtils mounts the routes below as not-implemented stubs until their task lands; the group is gated
// by leadgenRoutesEnabled, so the stubs are unreachable in production.
func registerLeadUtils(r *httpx.Router, _ Deps) {
	r.Handle("POST /api/leads/score", notImplemented("POST /api/leads/score"))
	r.Handle("POST /api/leads/verify-email", notImplemented("POST /api/leads/verify-email"))
	r.Handle("POST /api/leads/tech-stack", notImplemented("POST /api/leads/tech-stack"))
}
