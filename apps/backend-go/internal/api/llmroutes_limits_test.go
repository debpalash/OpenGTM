package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/nlcolumn"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func TestCampaignGenerateCapsFreeText(t *testing.T) {
	h := newTestRoutes(&fakeLeadStore{}, &scriptLLM{})
	for _, field := range []string{"value_proposition", "extra_context", "tone"} {
		body := `{"lead_ids":[1],"value_proposition":"vp","` + field + `":"` + strings.Repeat("a", 5000) + `"}`
		rec := serve(h.campaignGenerate, "POST", "/api/campaigns/generate", body)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), "string_too_long") {
			t.Errorf("%s: status %d body %.200s", field, rec.Code, rec.Body)
		}
	}
	body := `{"lead_ids":[],"value_proposition":"` + strings.Repeat("a", 2000) + `","extra_context":"` + strings.Repeat("b", 4000) + `"}`
	if rec := serve(h.campaignGenerate, "POST", "/api/campaigns/generate", body); rec.Code != 200 {
		t.Errorf("at the cap: status %d %.200s", rec.Code, rec.Body)
	}
}

func TestLLMRoutesAreRateLimitedPerWorkspace(t *testing.T) {
	h := newTestRoutes(&fakeLeadStore{}, &scriptLLM{})
	h.d.Limiter = ratelimit.New(nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	for _, spec := range []string{llmGenerateRate} {
		lim := h.limit(spec, next)
		code := func(user int32) int {
			req := httptest.NewRequest("POST", "/x", nil)
			req = req.WithContext(identity.WithPrincipal(req.Context(), identity.Principal{User: identity.User{ID: user}}))
			rec := httptest.NewRecorder()
			lim.ServeHTTP(rec, req)
			return rec.Code
		}
		n := 0
		for code(1) == 200 && n < 1000 {
			n++
		}
		if n == 0 || n >= 1000 {
			t.Fatalf("limit not applied: %d requests passed", n)
		}
		if code(2) != 200 {
			t.Error("another caller shares the budget")
		}
	}
}

func TestGenerateColumnDroppedResponse(t *testing.T) {
	resp := columnResponse(nlcolumn.Result{Kind: "http", Explanation: "e", Dropped: true, Warnings: []string{"w"}})
	b := llmRespJSON(t, resp)
	if b != `{"column":null,"kind":"http","explanation":"e","warnings":["w"]}` {
		t.Errorf("got %s", b)
	}
	kept := columnResponse(nlcolumn.Result{Column: wbcrud.MakeColumn("id", "a", "type", "formula"), Kind: "formula", Explanation: "e"})
	b = llmRespJSON(t, kept)
	if strings.Contains(b, "warnings") || strings.Contains(b, `"column":null`) {
		t.Errorf("kept response: %s", b)
	}
}

func llmRespJSON(t *testing.T, v any) string {
	t.Helper()
	rec := httptest.NewRecorder()
	writePy(rec, v)
	return strings.TrimSpace(rec.Body.String())
}
