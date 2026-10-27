//go:build integration

package wbcrud_test

import (
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const outBad = `{"id":"o","name":"O","type":"output","destination":"webhook","destination_config":{"url":"https://{host}.example.com/x"}}`
const outOK = `{"id":"o","name":"O","type":"output","destination":"webhook","destination_config":{"url":"https://h.example.com/x/{company}"}}`

func wantOutputGuard(t *testing.T, err error) {
	t.Helper()
	g, ok := err.(*wbcrud.GuardError)
	if !ok || g.Status != 422 || !strings.HasPrefix(g.Detail, "Output column o:") {
		t.Fatalf("want 422 output guard, got %v", err)
	}
}

// Every save path of an output column enforces the webhook URL rule (plan 15 small fixes).
func TestOutputWebhookURLRuleAtSave(t *testing.T) {
	s, ctx := setup(t)
	base := `{"id":"company","name":"Company","lead_field":"company"}`

	_, err := s.Create(ctx, wsA, mustBody(t, `{"name":"O1","columns_config":[`+base+`,`+outBad+`]}`))
	wantOutputGuard(t, err)

	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"O2","columns_config":[`+base+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(ctx, wsA, wb.ID, updateBody(t, `{"columns_config":[`+base+`,`+outBad+`]}`))
	wantOutputGuard(t, err)

	col, errs := wbcrud.ParseAddBody([]byte(outBad))
	if len(errs) > 0 {
		t.Fatalf("%+v", errs)
	}
	_, err = s.AddColumn(ctx, wsA, wb.ID, col)
	wantOutputGuard(t, err)

	good, errs := wbcrud.ParseAddBody([]byte(outOK))
	if len(errs) > 0 {
		t.Fatalf("%+v", errs)
	}
	if _, err = s.AddColumn(ctx, wsA, wb.ID, good); err != nil {
		t.Fatal(err)
	}
	sb, errs := wbcrud.ParseSettingsBody([]byte(`{"changes":{"destination_config":{"url":"https://{host}.example.com/x"}},"expected":{"destination_config":{"url":"https://h.example.com/x/{company}"}}}`))
	if len(errs) > 0 {
		t.Fatalf("%+v", errs)
	}
	_, err = s.UpdateColumnSettings(ctx, wsA, wb.ID, "o", sb)
	wantOutputGuard(t, err)
}
