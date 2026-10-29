package leadpeople

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

type fakeSearch struct {
	hits    map[string][]any
	mu      sync.Mutex
	queries [][2]any
	delay   time.Duration
}

func (f *fakeSearch) Text(ctx context.Context, q string, o leadweb.SearchOptions) []leadweb.Hit {
	f.mu.Lock()
	f.queries = append(f.queries, [2]any{q, o.MaxResults})
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil
		}
	}
	var out []leadweb.Hit
	for _, h := range asMaps(f.hits[q]) {
		// Recorded hits may use url/content instead of href/body (the Python
		// `hit.get("href") or hit.get("url")` fallback); leadweb.Hit has one spelling.
		href, _ := h["href"].(string)
		if href == "" {
			href, _ = h["url"].(string)
		}
		body, _ := h["body"].(string)
		if body == "" {
			body, _ = h["content"].(string)
		}
		title, _ := h["title"].(string)
		out = append(out, leadweb.Hit{Href: href, Title: title, Body: body})
	}
	return out
}

func TestVerifyVectors(t *testing.T) {
	var cases []struct {
		Company   string           `json:"company"`
		Function  string           `json:"function"`
		People    []any            `json:"people"`
		MaxPeople int              `json:"max_people"`
		Hits      map[string][]any `json:"hits"`
		Queries   [][2]any         `json:"queries"`
		Result    any              `json:"result"`
	}
	loadVec(t, "verify.json", &cases)
	for i, c := range cases {
		fs := &fakeSearch{hits: c.Hits, queries: [][2]any{}}
		s := &Service{Search: fs, Now: func() time.Time { return fixedNow }, Concurrency: 1}
		got, err := s.VerifyPeopleAtCompany(context.Background(), c.Company, c.Function, asMaps(c.People), c.MaxPeople)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "verify#"+string(rune('a'+i)), got, c.Result)
		sameJSON(t, "queries", fs.queries, c.Queries)
	}
}

func TestVerifyTimeoutMarksEveryone(t *testing.T) {
	fs := &fakeSearch{hits: map[string][]any{}, delay: time.Minute}
	s := &Service{Search: fs, Now: func() time.Time { return fixedNow }, VerifyTimeout: 30 * time.Millisecond}
	got, err := s.VerifyPeopleAtCompany(context.Background(), "Stripe", "Partnerships",
		[]map[string]any{{"name": "Jane Doe"}, {"name": "John Roe"}, {"name": ""}}, 15)
	if err != nil {
		t.Fatal(err)
	}
	sum := got["summary"].(map[string]any)
	if sum["verification_timeout"] != 3 || sum["not_corroborated"] != 0 {
		t.Fatalf("summary = %v", sum)
	}
	for _, p := range got["people"].([]map[string]any) {
		if p["verification_status"] != "verification_timeout" || p["verification_confidence"] != 0.0 || p["person_id"] == nil {
			t.Fatalf("person = %v", p)
		}
	}
}

func TestVerifyCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Service{Search: &fakeSearch{hits: map[string][]any{}}}
	if _, err := s.VerifyPeopleAtCompany(ctx, "Stripe", "Sales", []map[string]any{{"name": "A B"}}, 15); err == nil {
		t.Fatal("want ctx error")
	}
}
