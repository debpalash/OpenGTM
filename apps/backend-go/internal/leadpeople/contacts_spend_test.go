package leadpeople

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeContactSpend records the ledger traffic of a contacts action.
type fakeContactSpend struct {
	mu        sync.Mutex
	refuse    map[string]string // provider -> refusal reason
	reserved  []ContactAttempt
	settled   map[string]int64
	uncertain []string
	released  []string
}

type fakeContactHold struct {
	s *fakeContactSpend
	a ContactAttempt
}

func (f *fakeContactSpend) Reserve(_ context.Context, a ContactAttempt) (ContactHold, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.refuse[a.Provider]; r != "" {
		return nil, &SpendRefusedError{Reason: r}
	}
	f.reserved = append(f.reserved, a)
	return &fakeContactHold{f, a}, nil
}

func (h *fakeContactHold) key() string { return h.a.PersonID + "/" + h.a.Provider }
func (h *fakeContactHold) Dispatch(ctx context.Context) error {
	return ctx.Err()
}
func (h *fakeContactHold) Release(context.Context) {
	h.s.mu.Lock()
	h.s.released = append(h.s.released, h.key())
	h.s.mu.Unlock()
}
func (h *fakeContactHold) Settle(_ context.Context, c int64, _ string, _ map[string]any) {
	h.s.mu.Lock()
	if h.s.settled == nil {
		h.s.settled = map[string]int64{}
	}
	h.s.settled[h.a.Provider] += c
	h.s.mu.Unlock()
}
func (h *fakeContactHold) Uncertain(context.Context) {
	h.s.mu.Lock()
	h.s.uncertain = append(h.s.uncertain, h.a.Provider)
	h.s.mu.Unlock()
}

var stripeRes = map[string]any{"canonical_domain": "stripe.com"}

func found(email string) map[string]any {
	return map[string]any{"success": true, "confidence": 0.9,
		"fields": map[string]any{"email": email, "email_match_method": "exact_name"}}
}

// M4: each paid discovery and the verification are reserved at the catalog price before
// the call and settled after it; a failed dispatch is uncertain.
func TestContactsPaidCallsGoThroughLedger(t *testing.T) {
	sp := &fakeContactSpend{}
	disc := &fakeDisc{script: map[string]any{
		"prospeo":   map[string]any{"raise": "runtime"},
		"hunter_io": found("jane@stripe.com"),
	}}
	ver := &fakeVerify{script: map[string]any{"*": map[string]any{"status": "valid", "confidence": "verified", "source": "reacher"}}}
	s := &Service{Discover: disc, Verify: ver, Spend: sp, Concurrency: 1}
	got, err := s.EnrichPeopleContacts(context.Background(), "Stripe", "Sales", []map[string]any{{"name": "Jane Doe"}},
		ContactOptions{CompanyResolution: stripeRes, WorkspaceID: "w"})
	if err != nil || got["ok"] != true {
		t.Fatalf("%v %v", got, err)
	}
	if len(sp.reserved) != 3 {
		t.Fatalf("reserved %+v", sp.reserved)
	}
	if a := sp.reserved[0]; a.Provider != "prospeo" || a.Stage != "discovery" || a.ExposureMicroUSD != 20000 || a.ActionID != got["action_id"] {
		t.Fatalf("first attempt %+v", a)
	}
	if a := sp.reserved[1]; a.Provider != "hunter_io" || a.ExposureMicroUSD != 40000 {
		t.Fatalf("second attempt %+v", a)
	}
	if a := sp.reserved[2]; a.Stage != "verification" {
		t.Fatalf("third attempt %+v", a)
	}
	if fmt.Sprint(sp.uncertain) != "[prospeo]" || sp.settled["hunter_io"] != 40000 {
		t.Fatalf("uncertain %v settled %v", sp.uncertain, sp.settled)
	}
	if _, ok := sp.settled["verification_cascade"]; !ok {
		t.Fatalf("verification not settled: %v", sp.settled)
	}
}

// M4: a refused reservation means the provider is never called.
func TestContactsRefusedReservationSkipsProvider(t *testing.T) {
	sp := &fakeContactSpend{refuse: map[string]string{"prospeo": "cell_budget"}}
	disc := &fakeDisc{script: map[string]any{"hunter_io": found("jane@stripe.com")}}
	ver := &fakeVerify{script: map[string]any{"*": map[string]any{"status": "valid", "confidence": "verified", "source": "reacher"}}}
	s := &Service{Discover: disc, Verify: ver, Spend: sp, Concurrency: 1}
	got, err := s.EnrichPeopleContacts(context.Background(), "Stripe", "Sales", []map[string]any{{"name": "Jane Doe"}},
		ContactOptions{CompanyResolution: stripeRes, WorkspaceID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.calls) != 1 || disc.calls[0].Provider != "hunter_io" {
		t.Fatalf("calls %+v", disc.calls)
	}
	p := got["people"].([]map[string]any)[0]["contactability"].(map[string]any)
	a := p["attempts"].([]map[string]any)[0]
	if a["status"] != "blocked" || a["detail"] != "spend_cell_budget" {
		t.Fatalf("attempt %v", a)
	}
}

// M4: without a ledger no paid lookup runs (fail closed).
func TestContactsWithoutLedgerFailClosed(t *testing.T) {
	disc := &fakeDisc{script: map[string]any{"hunter_io": found("jane@stripe.com")}}
	s := &Service{Discover: disc, Verify: &fakeVerify{}, Concurrency: 1}
	got, err := s.EnrichPeopleContacts(context.Background(), "Stripe", "Sales", []map[string]any{{"name": "Jane Doe"}},
		ContactOptions{CompanyResolution: stripeRes, WorkspaceID: "w"})
	if err != nil || got["ok"] != true || len(disc.calls) != 0 {
		t.Fatalf("%v %v calls=%d", got, err, len(disc.calls))
	}
}

// M4: the number of selected people per call is capped (default 25).
func TestContactsSelectionCap(t *testing.T) {
	people := make([]map[string]any, 26)
	for i := range people {
		people[i] = map[string]any{"name": fmt.Sprintf("Person %d", i)}
	}
	disc := &fakeDisc{script: map[string]any{}}
	s := &Service{Discover: disc, Verify: &fakeVerify{}, Spend: &fakeContactSpend{}}
	got, err := s.EnrichPeopleContacts(context.Background(), "Stripe", "Sales", people,
		ContactOptions{CompanyResolution: stripeRes, WorkspaceID: "w"})
	if err != nil || got["ok"] != false || got["error"] != "Too many people selected" || got["max_people"] != 25 || got["count"] != 26 {
		t.Fatalf("%v %v", got, err)
	}
	if len(disc.calls) != 0 {
		t.Fatalf("calls %d", len(disc.calls))
	}
	s.MaxPeople = 2
	got, _ = s.EnrichPeopleContacts(context.Background(), "Stripe", "Sales", people[:3],
		ContactOptions{CompanyResolution: stripeRes, WorkspaceID: "w"})
	if got["ok"] != false || got["max_people"] != 2 {
		t.Fatalf("%v", got)
	}
	t.Setenv("PEOPLE_CONTACTS_MAX_SELECTED", "3")
	s.MaxPeople = 0
	got, _ = s.EnrichPeopleContacts(context.Background(), "Stripe", "Sales", people[:3],
		ContactOptions{CompanyResolution: stripeRes, WorkspaceID: "w"})
	if got["ok"] != true {
		t.Fatalf("%v", got)
	}
}

// M4: a hold whose dispatch fails (cancelled before the call) is released, not uncertain.
func TestContactsCancelBeforeDispatchReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sp := &fakeContactSpend{}
	s := &Service{Discover: &fakeDisc{}, Verify: &fakeVerify{}, Spend: &cancelOnReserve{sp, cancel}, Concurrency: 1}
	_, err := s.EnrichPeopleContacts(ctx, "Stripe", "Sales", []map[string]any{{"name": "Jane Doe"}},
		ContactOptions{CompanyResolution: stripeRes, WorkspaceID: "w"})
	if err == nil || len(sp.released) != 1 || len(sp.uncertain) != 0 {
		t.Fatalf("err %v released %v uncertain %v", err, sp.released, sp.uncertain)
	}
}

type cancelOnReserve struct {
	*fakeContactSpend
	cancel func()
}

func (c *cancelOnReserve) Reserve(ctx context.Context, a ContactAttempt) (ContactHold, error) {
	h, err := c.fakeContactSpend.Reserve(ctx, a)
	c.cancel()
	return h, err
}

var _ = time.Second
