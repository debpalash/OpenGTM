package scrape

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// optSearch answers every query with n LinkedIn-style people hits / job hits and records the
// requested result counts.
type optSearch struct {
	mu   sync.Mutex
	n    int
	maxs []int
}

func (s *optSearch) Text(_ context.Context, q string, max int) []Hit {
	s.mu.Lock()
	s.maxs = append(s.maxs, max)
	s.mu.Unlock()
	var out []Hit
	for i := 0; i < s.n; i++ {
		out = append(out, Hit{Title: fmt.Sprintf("Person%c Example - CEO - Acme | LinkedIn", 'A'+i),
			Href: fmt.Sprintf("https://www.linkedin.com/in/person-%d", i), Body: "Acme"})
	}
	return out
}

func (s *optSearch) Web(ctx context.Context, q string, max int, _ bool) ([]Hit, error) {
	return s.Text(ctx, q, max), nil
}

type optMail struct{ picked [][]string }

func (m *optMail) HasMX(context.Context, string) bool { return true }
func (m *optMail) PickBest(_ context.Context, c []string) (MailCheck, error) {
	m.picked = append(m.picked, c)
	return MailCheck{}, nil
}
func (m *optMail) Verify(context.Context, string) (MailCheck, error) {
	f := false
	return MailCheck{Deliverable: &f}, nil
}

// The collect stages pass the job's provider settings (Python constructed CrossLinkedProvider(max_people=3,
// delay=1.5), JobSpySignalProvider(max_jobs=3, delay=1.5), MailScoutVerifyProvider(max_verify=2)) as
// per-call options in Input.Settings; out-of-range values are clamped.
func TestProviderCallOptions(t *testing.T) {
	t.Cleanup(func() { Configure(Deps{}) })
	var mu sync.Mutex
	var slept []time.Duration
	s := &optSearch{n: 1}
	mail := &optMail{}
	Configure(Deps{Search: s, Mail: mail, Sleep: func(_ context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return nil
	}})
	ctx := context.Background()
	lead := map[string]any{"company": "Acme", "email": "x@acme.com", "contact_person": "Jane Doe", "website": "https://acme.com"}

	// crosslinked: delay 1.5 s between searches, at most 3 people.
	in := providers.Input{Lead: lead, Settings: map[string]any{"max_people": 3, "delay_ms": 1500}}
	if _, err := (CrossLinked{}).Enrich(ctx, nil, nil, in); err != nil {
		t.Fatal(err)
	}
	if len(slept) == 0 || slept[0] != 1500*time.Millisecond {
		t.Errorf("crosslinked delays %v, want 1.5s", slept)
	}
	s.n = 8
	out, _ := (CrossLinked{}).Enrich(ctx, nil, nil, in)
	var dms []any
	dm, _ := out.Extra.Get("decision_makers")
	_ = json.Unmarshal([]byte(dm.(string)), &dms)
	if len(dms) != 3 {
		t.Errorf("crosslinked max_people 3 -> %d people", len(dms))
	}
	in.Settings["max_people"] = 1000
	out, _ = (CrossLinked{}).Enrich(ctx, nil, nil, in)
	dm, _ = out.Extra.Get("decision_makers")
	_ = json.Unmarshal([]byte(dm.(string)), &dms)
	if len(dms) > 10 {
		t.Errorf("max_people not clamped: %d", len(dms))
	}

	// jobspy: 3 results per query, 1.5 s between queries.
	slept, s.maxs = nil, nil
	if _, err := (JobSpy{}).Enrich(ctx, nil, nil, providers.Input{Lead: lead,
		Settings: map[string]any{"max_jobs": 3, "delay_ms": 1500}}); err != nil {
		t.Fatal(err)
	}
	if len(s.maxs) == 0 || s.maxs[0] != 3 || len(slept) == 0 || slept[0] != 1500*time.Millisecond {
		t.Errorf("jobspy max %v delays %v", s.maxs, slept)
	}

	// mailscout: at most 2 personal patterns verified.
	if _, err := (mailScout{}).Enrich(ctx, nil, nil, providers.Input{Lead: lead,
		Settings: map[string]any{"max_verify": 2}}); err != nil {
		t.Fatal(err)
	}
	if len(mail.picked) != 1 || len(mail.picked[0]) != 2 {
		t.Errorf("mailscout verified %v, want 2 patterns", mail.picked)
	}
}
