package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
)

// In-memory fakes of the ports for tool and pipeline tests. Zero values work;
// the tx argument is ignored. Err, when set, is returned by every call.

type fakeLeads struct {
	mu      sync.Mutex
	Rows    map[int64]*Lead
	NextID  int64
	StatsV  StatsResult
	Err     error
	Deleted []int64
}

func (f *fakeLeads) List(_ context.Context, _ pgx.Tx, p ListParams) ([]Lead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	ids := make([]int64, 0, len(f.Rows))
	for id := range f.Rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []Lead
	for _, id := range ids {
		if p.Limit > 0 && len(out) >= p.Limit {
			break
		}
		out = append(out, *f.Rows[id])
	}
	return out, nil
}
func (f *fakeLeads) ByID(_ context.Context, _ pgx.Tx, id int64) (*Lead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Rows[id], nil
}
func (f *fakeLeads) Stats(context.Context, pgx.Tx) (StatsResult, error) { return f.StatsV, f.Err }
func (f *fakeLeads) Upsert(_ context.Context, _ pgx.Tx, fl LeadFields) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return 0, f.Err
	}
	if f.Rows == nil {
		f.Rows = map[int64]*Lead{}
	}
	f.NextID++
	l := &Lead{ID: f.NextID}
	if c, ok := fl["company"].(string); ok {
		l.Company = c
	}
	f.Rows[l.ID] = l
	return l.ID, nil
}
func (f *fakeLeads) UpdateFields(context.Context, pgx.Tx, int64, LeadFields) error { return f.Err }
func (f *fakeLeads) SetStatus(context.Context, pgx.Tx, int64, string) error        { return f.Err }
func (f *fakeLeads) Delete(_ context.Context, _ pgx.Tx, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Rows, id)
	f.Deleted = append(f.Deleted, id)
	return f.Err
}

type fakeEnrich struct {
	Verify VerifyResult
	PyOut  map[string]json.RawMessage // by provider
	Err    error
	Calls  []string
	Scopes []provcall.SpendScope // the ledger scope each Py call carried
}

func (f *fakeEnrich) VerifyEmail(_ context.Context, _, email string) (VerifyResult, error) {
	f.Calls = append(f.Calls, "verify:"+email)
	return f.Verify, f.Err
}
func (f *fakeEnrich) Py(ctx context.Context, _, provider string, _ any) (json.RawMessage, error) {
	sc, _ := provcall.SpendScopeFrom(ctx)
	f.Scopes = append(f.Scopes, sc)
	f.Calls = append(f.Calls, "py:"+provider)
	return f.PyOut[provider], f.Err
}

type fakeWorkbooks struct {
	Created map[string][]Column
	Err     error
	n       int
}

func (f *fakeWorkbooks) Create(_ context.Context, _ pgx.Tx, _, name, _ string, cols []Column) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	if f.Created == nil {
		f.Created = map[string][]Column{}
	}
	f.n++
	id := name + "#" + string(rune('0'+f.n))
	f.Created[id] = cols
	return id, nil
}
func (f *fakeWorkbooks) Delete(_ context.Context, _ pgx.Tx, _, id string) (bool, error) {
	_, ok := f.Created[id]
	delete(f.Created, id)
	return ok, f.Err
}

type fakeOutreach struct {
	Sequences  map[string]bool
	Suppressed map[string]bool
	Enrolled   map[string]bool // seqID+"/"+leadID
	Sent       []QueueSendRequest
	SendRes    SendResult
	Err        error
}

func (f *fakeOutreach) SequenceExists(_ context.Context, _ pgx.Tx, _, seq string) (bool, error) {
	return f.Sequences[seq], f.Err
}
func (f *fakeOutreach) IsSuppressed(_ context.Context, _ pgx.Tx, _, email string) (bool, error) {
	return f.Suppressed[email], f.Err
}
func (f *fakeOutreach) Enroll(_ context.Context, _ pgx.Tx, _, seq string, lead int64, _, _ string) (bool, error) {
	if f.Err != nil {
		return false, f.Err
	}
	if f.Enrolled == nil {
		f.Enrolled = map[string]bool{}
	}
	k := seq + "/" + string(rune('0'+lead))
	if f.Enrolled[k] {
		return false, nil
	}
	f.Enrolled[k] = true
	return true, nil
}
func (f *fakeOutreach) QueueSend(_ context.Context, _ pgx.Tx, _ string, r QueueSendRequest) (SendResult, error) {
	f.Sent = append(f.Sent, r)
	return f.SendRes, f.Err
}

type fakeAutomation struct {
	Rules     int
	Reject    *HTTPError
	Stored    json.RawMessage
	Bootstrap []string
	Err       error
}

func (f *fakeAutomation) CountRules(context.Context, pgx.Tx, string) (int, error) {
	return f.Rules, f.Err
}
func (f *fakeAutomation) ValidateRule(context.Context, pgx.Tx, string, RuleInput) *HTTPError {
	return f.Reject
}
func (f *fakeAutomation) CreateTrigger(context.Context, pgx.Tx, string, RuleInput) (json.RawMessage, error) {
	f.Rules++
	return f.Stored, f.Err
}
func (f *fakeAutomation) BootstrapSchedule(_ context.Context, _, id string) error {
	f.Bootstrap = append(f.Bootstrap, id)
	return f.Err
}

var (
	_ LeadPort       = (*fakeLeads)(nil)
	_ EnrichPort     = (*fakeEnrich)(nil)
	_ WorkbookPort   = (*fakeWorkbooks)(nil)
	_ OutreachPort   = (*fakeOutreach)(nil)
	_ AutomationPort = (*fakeAutomation)(nil)
)

// nilTxRunner is a Deps.InWorkspace for tests: fn runs with a nil tx.
func nilTxRunner(_ context.Context, _ string, fn func(pgx.Tx) error) error { return fn(nil) }
