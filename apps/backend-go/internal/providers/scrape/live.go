package scrape

import (
	"context"
	"errors"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// LiveSearch adapts leadweb.Searcher: Text is the cached resilient search
// salted with the calling workspace, Web the SearXNG-first web_search.
type LiveSearch struct{ S *leadweb.Searcher }

func (l LiveSearch) Text(ctx context.Context, q string, n int) []Hit {
	return l.S.Text(ctx, q, leadweb.SearchOptions{MaxResults: n, Workspace: providers.WorkspaceFrom(ctx)})
}

func (l LiveSearch) Web(ctx context.Context, q string, n int, raise bool) ([]Hit, error) {
	return l.S.WebSearch(ctx, q, n, raise)
}

// LiveMail adapts emailverify (SMTP over the egress dial guard).
type LiveMail struct{ D emailverify.Deps }

func check(c emailverify.EmailCheck) MailCheck {
	return MailCheck{Email: c.Email, Confidence: c.Confidence, Deliverable: c.Deliverable, CatchAll: c.CatchAll}
}

func (m LiveMail) HasMX(ctx context.Context, domain string) bool {
	return emailverify.HasMX(ctx, m.D, domain)
}

func (m LiveMail) PickBest(ctx context.Context, c []string) (MailCheck, error) {
	r, err := emailverify.PickBestEmail(ctx, m.D, c)
	return check(r), err
}

func (m LiveMail) Verify(ctx context.Context, email string) (MailCheck, error) {
	r, err := emailverify.VerifyEmail(ctx, m.D, email)
	return check(r), err
}

var _ Browser = (*browserclient.Client)(nil)

func asBrowserErr(err error, target **browserclient.Error) bool { return errors.As(err, target) }
