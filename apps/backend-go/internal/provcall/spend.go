package provcall

import "context"

// SpendScope marks provider calls made outside any workbook (MCP enrich_company, the intent poller, signal scans,
// collection stages) as paid work for the workspace-level spend ledger (spend.WorkspaceLedger): a runner that is
// configured with a ledger reserves a paid call under this scope before dispatching it and settles it after.
// Calls without a scope (workbook cells, which carry their own ledger entries) are not ledgered here.
type SpendScope struct {
	// Run and Row identify the logical operation (for example "mcp:enrich_company:<id>" and "company:<domain>").
	Run, Row string
	// LimitUSD bounds the summed exposure of every paid call of the scope's cell; 0 means DefaultSpendLimitUSD.
	LimitUSD float64
}

// DefaultSpendLimitUSD bounds one scope when it names no limit.
const DefaultSpendLimitUSD = 1.0

type spendKey struct{}

// WithSpendScope returns ctx marked with s.
func WithSpendScope(ctx context.Context, s SpendScope) context.Context {
	return context.WithValue(ctx, spendKey{}, s)
}

// SpendScopeFrom returns the scope set by WithSpendScope.
func SpendScopeFrom(ctx context.Context) (SpendScope, bool) {
	s, ok := ctx.Value(spendKey{}).(SpendScope)
	return s, ok
}
