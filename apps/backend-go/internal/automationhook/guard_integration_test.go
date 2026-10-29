//go:build integration

package automationhook

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// A panicking emitter must not escape into the caller's row-write transaction:
// the rule is skipped (0), the savepoint is rolled back, the panic value (which
// may carry row data) is not logged, and the caller's tx stays usable.
func TestGuardedRecoversPanicAndKeepsCallerTx(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	var logs bytes.Buffer
	h := &hook{enabled: true, log: slog.New(slog.NewTextHandler(&logs, nil))}
	err := tenancy.InWorkspace(ctx, pool, "ws-ah", func(tx pgx.Tx) error {
		n := h.guarded(ctx, tx, "row-added", "ws-ah", func(sp pgx.Tx) (int, error) {
			panic("secret-cell-value-xyz")
		})
		if n != 0 {
			t.Errorf("n=%d want 0", n)
		}
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one) // fails if the tx is aborted or the savepoint leaked
	})
	if err != nil {
		t.Fatalf("caller tx unusable after panic: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "panic") {
		t.Errorf("panic not logged: %q", out)
	}
	if strings.Contains(out, "secret-cell-value-xyz") {
		t.Errorf("panic value leaked into log: %q", out)
	}
}
