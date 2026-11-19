package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestDetailWithNextStepKeepsBodyAndSetsHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	Detail(rec, 400, "SMTP not configured", StepEmail...)
	if got := rec.Body.String(); got != `{"detail":"SMTP not configured"}` {
		t.Fatalf("body changed: %s", got)
	}
	if got := rec.Header().Get(NextStepHeader); got != "/settings?tab=email" {
		t.Errorf("%s = %q", NextStepHeader, got)
	}
	if got := rec.Header().Get(NextStepLabelHeader); got != "Open email settings" {
		t.Errorf("%s = %q", NextStepLabelHeader, got)
	}
}
