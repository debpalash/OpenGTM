package api

import (
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Applying a function whose output column has a placeholder in its webhook host is refused (422).
func TestApplyValidatorRefusesNonLiteralWebhookHost(t *testing.T) {
	before := `[{"id":"a","name":"A"}]`
	bad := `[{"id":"a","name":"A"},{"id":"o","name":"O","type":"output","destination":"webhook","destination_config":{"url":"https://{h}.example.com/x"}}]`
	err := noNewCycles([]byte(before), []byte(bad))
	var g *wbcrud.GuardError
	if !errors.As(err, &g) || g.Status != 422 {
		t.Fatalf("got %v", err)
	}
	ok := `[{"id":"a","name":"A"},{"id":"o","name":"O","type":"output","destination":"webhook","destination_config":{"url":"https://h.example.com/{a}"}}]`
	if err := noNewCycles([]byte(before), []byte(ok)); err != nil {
		t.Fatal(err)
	}
}
