package collect

import (
	"context"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

type searchOnlyWeb struct{ hits []leadweb.Hit }

func (w searchOnlyWeb) Search(context.Context, string, int) []leadweb.Hit { return w.hits }
func (searchOnlyWeb) Fetch(context.Context, string, int, time.Duration) (string, bool) {
	return "", false
}

// A personal address found by web search is labelled "found", never
// "verified": nothing confirmed the mailbox (honesty rule).
func TestFindPersonalEmailSearchHitIsFoundNotVerified(t *testing.T) {
	w := searchOnlyWeb{hits: []leadweb.Hit{{Title: "Jane Doe - Acme", Body: "Contact jane.doe@acme.io for sales"}}}
	email, conf := FindPersonalEmail(context.Background(), w, nil, "Jane Doe", "Acme", "acme.io")
	if email != "jane.doe@acme.io" || conf != "found" {
		t.Fatalf("FindPersonalEmail = %q, %q; want jane.doe@acme.io, found", email, conf)
	}
}
