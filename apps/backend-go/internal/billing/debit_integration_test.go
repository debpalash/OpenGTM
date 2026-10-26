//go:build integration

package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func debit(t *testing.T, pool *pgxpool.Pool, enabled bool, ws string, cost float64, runID string) (DebitResult, error) {
	t.Helper()
	var res DebitResult
	err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) (e error) {
		res, e = CheckAndDebit(context.Background(), tx, enabled, ws, cost, runID, "run_debit")
		return e
	})
	return res, err
}

type ledgerRow struct {
	Amount float64
	Reason string
	RunID  *string
	Key    *string
}

func ledger(t *testing.T, pool *pgxpool.Pool, ws string) (balance float64, rows []ledgerRow) {
	t.Helper()
	err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(context.Background(), `SELECT balance_usd FROM workspace_credits WHERE workspace_id=$1`, ws).Scan(&balance)
		if errors.Is(err, pgx.ErrNoRows) {
			balance = -1 // marker: no balance row
		} else if err != nil {
			return err
		}
		r, err := tx.Query(context.Background(), `SELECT amount_usd, reason, run_id, idempotency_key FROM credit_ledger_entries WHERE workspace_id=$1 ORDER BY id`, ws)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var lr ledgerRow
			if err := r.Scan(&lr.Amount, &lr.Reason, &lr.RunID, &lr.Key); err != nil {
				return err
			}
			rows = append(rows, lr)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	return
}

func TestDebitDisabledIsNoop(t *testing.T) {
	pool := ledgerPool(t, "t6-dis")
	credit(t, pool, "t6-dis", 5, "manual_grant", "")
	for _, run := range []string{"r1", "r1"} {
		res, err := debit(t, pool, false, "t6-dis", 1000, run)
		if err != nil || res.ChargedUSD != 0 || res.BalanceUSD != 5 || res.IdempotentReplay {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	}
	bal, rows := ledger(t, pool, "t6-dis")
	if bal != 5 || len(rows) != 1 {
		t.Fatalf("bal=%v rows=%d", bal, len(rows))
	}
}

func TestDebitZeroAndNegativeAreNoops(t *testing.T) {
	pool := ledgerPool(t, "t6-zero", "t6-nobal")
	credit(t, pool, "t6-zero", 5, "manual_grant", "")
	for i, c := range []float64{0, -1, -4e-05, 1e-05, 4e-05} {
		res, err := debit(t, pool, true, "t6-zero", c, fmt.Sprintf("z%d", i))
		if err != nil || res.ChargedUSD != 0 || res.BalanceUSD != 5 || res.IdempotentReplay {
			t.Fatalf("cost %v: res=%+v err=%v", c, res, err)
		}
	}
	if _, rows := ledger(t, pool, "t6-zero"); len(rows) != 1 {
		t.Fatalf("noop debits wrote ledger rows: %d", len(rows))
	}
	// No balance row: balance 0, and still no row created.
	res, err := debit(t, pool, true, "t6-nobal", 0, "z")
	if err != nil || res.BalanceUSD != 0 || res.ChargedUSD != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if bal, rows := ledger(t, pool, "t6-nobal"); bal != -1 || len(rows) != 0 {
		t.Fatalf("noop created rows: bal=%v rows=%d", bal, len(rows))
	}
	// 5e-05 rounds (half-even on the binary value) up to 0.0001: charged.
	res, err = debit(t, pool, true, "t6-zero", 5e-05, "rt3")
	if err != nil || res.ChargedUSD != 0.0001 || res.BalanceUSD != 4.9999 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDebitReplaySameRunIDChargesOnce(t *testing.T) {
	pool := ledgerPool(t, "t6-rep")
	credit(t, pool, "t6-rep", 10, "manual_grant", "")
	res, err := debit(t, pool, true, "t6-rep", 2.5, "r1")
	if err != nil || res.ChargedUSD != 2.5 || res.BalanceUSD != 7.5 || res.IdempotentReplay {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// Replay ignores the new cost, even one the balance could not cover.
	for _, c := range []float64{2.5, 4, 1000} {
		res, err = debit(t, pool, true, "t6-rep", c, "r1")
		if err != nil || res.ChargedUSD != 0 || res.BalanceUSD != 7.5 || !res.IdempotentReplay {
			t.Fatalf("replay cost %v: res=%+v err=%v", c, res, err)
		}
	}
	bal, rows := ledger(t, pool, "t6-rep")
	if bal != 7.5 || len(rows) != 2 {
		t.Fatalf("bal=%v rows=%d", bal, len(rows))
	}
	if rows[1].Amount != -2.5 || rows[1].Reason != "run_debit" || rows[1].RunID == nil || *rows[1].RunID != "r1" {
		t.Fatalf("debit row %+v", rows[1])
	}
}

func TestDebitConcurrentSameRunIDChargesOnce(t *testing.T) {
	pool := ledgerPool(t, "t6-conc")
	credit(t, pool, "t6-conc", 10, "manual_grant", "")
	const n = 16
	var wg sync.WaitGroup
	results := make([]DebitResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = debit(t, pool, true, "t6-conc", 3, "same-run")
		}()
	}
	close(start)
	wg.Wait()
	charged, replays := 0, 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i].IdempotentReplay {
			replays++
			if results[i].ChargedUSD != 0 || results[i].BalanceUSD != 7 {
				t.Fatalf("replay result %+v", results[i])
			}
		} else {
			charged++
			if results[i].ChargedUSD != 3 || results[i].BalanceUSD != 7 {
				t.Fatalf("charged result %+v", results[i])
			}
		}
	}
	if charged != 1 || replays != n-1 {
		t.Fatalf("charged=%d replays=%d", charged, replays)
	}
	if bal, rows := ledger(t, pool, "t6-conc"); bal != 7 || len(rows) != 2 {
		t.Fatalf("bal=%v rows=%d", bal, len(rows))
	}
}

func TestDebitInsufficientWritesNothing(t *testing.T) {
	pool := ledgerPool(t, "t6-ins", "t6-ins-nobal")
	credit(t, pool, "t6-ins", 1, "manual_grant", "")
	_, err := debit(t, pool, true, "t6-ins", 2, "r1")
	var ice *InsufficientCreditsError
	if !errors.As(err, &ice) || ice.BalanceUSD != 1 || ice.RequiredUSD != 2 {
		t.Fatalf("err=%v", err)
	}
	want := "Insufficient credits: balance $1.0000 < required $2.0000. Top up to run this enrichment."
	if err.Error() != want {
		t.Fatalf("message %q", err.Error())
	}
	if bal, rows := ledger(t, pool, "t6-ins"); bal != 1 || len(rows) != 1 {
		t.Fatalf("bal=%v rows=%d", bal, len(rows))
	}
	// The failed run id was not consumed.
	if res, err := debit(t, pool, true, "t6-ins", 1, "r1"); err != nil || res.ChargedUSD != 1 || res.BalanceUSD != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// No balance row at all: 402-style error, no row created.
	_, err = debit(t, pool, true, "t6-ins-nobal", 0.5, "r1")
	if !errors.As(err, &ice) || ice.BalanceUSD != 0 || ice.RequiredUSD != 0.5 {
		t.Fatalf("err=%v", err)
	}
	if bal, rows := ledger(t, pool, "t6-ins-nobal"); bal != -1 || len(rows) != 0 {
		t.Fatalf("insufficient wrote: bal=%v rows=%d", bal, len(rows))
	}
}

func TestDebitRollsBackWithCallerTx(t *testing.T) {
	pool := ledgerPool(t, "t6-rb")
	credit(t, pool, "t6-rb", 10, "manual_grant", "")
	boom := errors.New("enqueue failed")
	err := tenancy.InWorkspace(context.Background(), pool, "t6-rb", func(tx pgx.Tx) error {
		res, e := CheckAndDebit(context.Background(), tx, true, "t6-rb", 4, "r1", "run_debit")
		if e != nil || res.ChargedUSD != 4 || res.BalanceUSD != 6 {
			t.Fatalf("res=%+v err=%v", res, e)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if bal, rows := ledger(t, pool, "t6-rb"); bal != 10 || len(rows) != 1 {
		t.Fatalf("debit survived rollback: bal=%v rows=%d", bal, len(rows))
	}
	// A retry with the same run id charges (nothing was consumed), once.
	if res, err := debit(t, pool, true, "t6-rb", 4, "r1"); err != nil || res.ChargedUSD != 4 || res.BalanceUSD != 6 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// Python's idempotency key is global across workspaces, so the same run id
// charged only the first workspace and replayed the rest. Go scopes it per
// workspace (documented deviation): both workspaces are charged, each once, and
// neither workspace's balance or ledger is visible to the other.
func TestDebitTenantIsolation(t *testing.T) {
	pool := ledgerPool(t, "t6-iso-a", "t6-iso-b")
	credit(t, pool, "t6-iso-a", 5, "manual_grant", "")
	credit(t, pool, "t6-iso-b", 5, "manual_grant", "")

	a, err := debit(t, pool, true, "t6-iso-a", 1, "same")
	if err != nil || a.ChargedUSD != 1 || a.BalanceUSD != 4 || a.IdempotentReplay {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	b, err := debit(t, pool, true, "t6-iso-b", 1, "same")
	if err != nil || b.ChargedUSD != 1 || b.BalanceUSD != 4 || b.IdempotentReplay {
		t.Fatalf("b=%+v err=%v (a run id used by another tenant must not replay)", b, err)
	}
	// Each workspace still replays its own run.
	for _, ws := range []string{"t6-iso-a", "t6-iso-b"} {
		r, err := debit(t, pool, true, ws, 1, "same")
		if err != nil || !r.IdempotentReplay || r.ChargedUSD != 0 || r.BalanceUSD != 4 {
			t.Fatalf("%s replay=%+v err=%v", ws, r, err)
		}
	}
	// Balances are independent: a's funds do not cover b's overspend.
	if _, err := debit(t, pool, true, "t6-iso-b", 4.5, "big"); err == nil {
		t.Fatal("expected insufficient credits")
	}
	// A different workspace cannot probe or consume another's run key.
	for _, ws := range []string{"t6-iso-a", "t6-iso-b"} {
		bal, rows := ledger(t, pool, ws)
		if bal != 4 || len(rows) != 2 {
			t.Fatalf("%s bal=%v rows=%d", ws, bal, len(rows))
		}
	}
}

func TestDebitWorkspaceIDMismatchRejected(t *testing.T) {
	pool := ledgerPool(t, "t6-mm-a", "t6-mm-b")
	credit(t, pool, "t6-mm-b", 5, "manual_grant", "")
	// tx scoped to a, debit asked for b: RLS hides b, so it must not charge b.
	err := tenancy.InWorkspace(context.Background(), pool, "t6-mm-a", func(tx pgx.Tx) error {
		_, e := CheckAndDebit(context.Background(), tx, true, "t6-mm-b", 1, "r1", "run_debit")
		return e
	})
	if err == nil {
		t.Fatal("expected an error for a cross-workspace debit")
	}
	if bal, rows := ledger(t, pool, "t6-mm-b"); bal != 5 || len(rows) != 1 {
		t.Fatalf("cross-workspace debit leaked: bal=%v rows=%d", bal, len(rows))
	}
}

type seqStep struct {
	Op             string          `json:"op"`
	Amount         float64         `json:"amount"`
	Cost           json.RawMessage `json:"cost"`
	RunID          string          `json:"run_id"`
	Workspace      string          `json:"workspace"`
	IdempotencyKey string          `json:"idempotency_key"`
	Result         struct {
		Value *struct {
			Charged float64 `json:"charged_usd"`
			Balance float64 `json:"balance_usd"`
			Replay  bool    `json:"idempotent_replay"`
		} `json:"value"`
		Error   string  `json:"error"`
		Message string  `json:"message"`
		Balance float64 `json:"balance_usd"`
		Req     float64 `json:"required_usd"`
	} `json:"result"`
	State struct {
		BalanceRow float64 `json:"balance_row"`
		Ledger     []struct {
			Amount float64 `json:"amount_usd"`
			Reason string  `json:"reason"`
			RunID  *string `json:"run_id"`
			Key    *string `json:"idempotency_key"`
		} `json:"ledger"`
	} `json:"state"`
}

func TestCheckAndDebitMatchesVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/debit_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sequences []struct {
			Name    string    `json:"name"`
			Enabled bool      `json:"billing_enabled"`
			Steps   []seqStep `json:"steps"`
		} `json:"sequences"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	pool := ledgerPool(t, "t6-vec", "t6-vec-a", "t6-vec-b")
	wsOf := func(s string) string {
		if s == "" {
			return "t6-vec"
		}
		return "t6-vec-" + s
	}
	ran := 0
	for _, seq := range doc.Sequences {
		t.Run(seq.Name, func(t *testing.T) {
			for _, w := range []string{"t6-vec", "t6-vec-a", "t6-vec-b"} {
				_ = tenancy.InWorkspace(context.Background(), pool, w, func(tx pgx.Tx) error {
					_, _ = tx.Exec(context.Background(), `DELETE FROM credit_ledger_entries WHERE workspace_id=$1`, w)
					_, _ = tx.Exec(context.Background(), `DELETE FROM workspace_credits WHERE workspace_id=$1`, w)
					return nil
				})
			}
			multi := false
			for i, st := range seq.Steps {
				ws := wsOf(st.Workspace)
				multi = multi || st.Workspace != ""
				switch st.Op {
				case "credit":
					var res DebitResult
					var cerr error
					cerr = tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) (e error) {
						res, e = Credit(context.Background(), tx, ws, st.Amount, "manual_grant", st.IdempotencyKey)
						return e
					})
					if st.Result.Value == nil {
						if cerr == nil {
							t.Fatalf("step %d: expected error %s", i, st.Result.Error)
						}
						continue
					}
					if cerr != nil {
						t.Fatalf("step %d: %v", i, cerr)
					}
					checkRes(t, i, res, st)
				case "debit":
					var cost float64
					var cs string
					if json.Unmarshal(st.Cost, &cs) == nil {
						c, perr := strconv.ParseFloat(cs, 64)
						if perr != nil {
							// Python ValueError on a non-numeric string: unrepresentable
							// with a float64 parameter (compile-time type safety).
							continue
						}
						cost = c
					} else if err := json.Unmarshal(st.Cost, &cost); err != nil {
						t.Fatalf("step %d cost: %v", i, err)
					}
					res, derr := debit(t, pool, seq.Enabled, ws, cost, st.RunID)
					if st.Result.Value == nil {
						var ice *InsufficientCreditsError
						if !errors.As(derr, &ice) {
							t.Fatalf("step %d: want InsufficientCreditsError, got %v", i, derr)
						}
						if ice.Error() != st.Result.Message || ice.BalanceUSD != st.Result.Balance || ice.RequiredUSD != st.Result.Req {
							t.Fatalf("step %d: %q (%v/%v) want %q", i, ice.Error(), ice.BalanceUSD, ice.RequiredUSD, st.Result.Message)
						}
						continue
					}
					if seq.Name == "two_workspaces_isolated" && i == 4 {
						// Documented deviation: Python's global key replays workspace
						// b's run "r2" for a; Go scopes the key, so a (balance 1) is
						// short for cost 3.
						var ice *InsufficientCreditsError
						if !errors.As(derr, &ice) || ice.BalanceUSD != 1 {
							t.Fatalf("deviation: res=%+v err=%v", res, derr)
						}
						continue
					}
					if derr != nil && seq.Name != "same_run_id_two_workspaces" {
						t.Fatalf("step %d: %v", i, derr)
					}
					if seq.Name == "same_run_id_two_workspaces" && ws == "t6-vec-b" {
						// Documented deviation: Python's global key replays; Go charges b.
						if res.IdempotentReplay || res.ChargedUSD != 1 || res.BalanceUSD != 4 {
							t.Fatalf("deviation: %+v", res)
						}
						continue
					}
					checkRes(t, i, res, st)
				default:
					t.Fatalf("unknown op %q", st.Op)
				}
				if multi {
					continue
				}
				bal, rows := ledger(t, pool, ws)
				if bal == -1 && len(rows) == 0 {
					bal = 0 // Python's get_balance creates a zero row; Go's no-op reads create nothing
				}
				if bal != st.State.BalanceRow || len(rows) != len(st.State.Ledger) {
					t.Fatalf("step %d state: bal=%v rows=%d want %v/%d", i, bal, len(rows), st.State.BalanceRow, len(st.State.Ledger))
				}
				for j, w := range st.State.Ledger {
					g := rows[j]
					if g.Amount != w.Amount || g.Reason != w.Reason || (g.RunID == nil) != (w.RunID == nil) ||
						(g.RunID != nil && *g.RunID != *w.RunID) {
						t.Fatalf("step %d row %d: %+v want %+v", i, j, g, w)
					}
					// Debit keys are workspace-scoped in Go but must still carry
					// Python's "run:<id>" token; other keys are untouched.
					if w.Key == nil {
						if g.Key != nil {
							t.Fatalf("step %d row %d: unexpected key %q", i, j, *g.Key)
						}
					} else if g.Key == nil || (*g.Key != *w.Key && !strings.Contains(*g.Key, *w.Key)) {
						t.Fatalf("step %d row %d: key %v want %q", i, j, g.Key, *w.Key)
					}
				}
			}
			ran++
		})
	}
	if ran < 16 {
		t.Fatalf("ran %d sequences", ran)
	}
}

func checkRes(t *testing.T, i int, got DebitResult, st seqStep) {
	t.Helper()
	w := st.Result.Value
	if got.ChargedUSD != w.Charged || got.BalanceUSD != w.Balance || got.IdempotentReplay != w.Replay {
		t.Fatalf("step %d: got %+v want %+v", i, got, *w)
	}
}

func insertRawEntry(t *testing.T, pool *pgxpool.Pool, ws, key string) {
	t.Helper()
	err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
		_, e := tx.Exec(context.Background(),
			`INSERT INTO credit_ledger_entries (workspace_id, amount_usd, reason, run_id, idempotency_key) VALUES ($1,-1,'run_debit','x',$2)`, ws, key)
		return e
	})
	if err != nil {
		t.Fatalf("insert raw entry: %v", err)
	}
}

// A legacy Python "run:<id>" row in the same workspace is a replay; the same
// run id in another workspace is a normal charge.
func TestDebitLegacyKeyFallback(t *testing.T) {
	pool := ledgerPool(t, "t6-leg-a", "t6-leg-b")
	credit(t, pool, "t6-leg-a", 5, "manual_grant", "")
	credit(t, pool, "t6-leg-b", 5, "manual_grant", "")
	insertRawEntry(t, pool, "t6-leg-a", "run:legacy1")

	a, err := debit(t, pool, true, "t6-leg-a", 2, "legacy1")
	if err != nil || !a.IdempotentReplay || a.ChargedUSD != 0 || a.BalanceUSD != 5 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	if bal, rows := ledger(t, pool, "t6-leg-a"); bal != 5 || len(rows) != 2 {
		t.Fatalf("replay wrote: bal=%v rows=%d", bal, len(rows))
	}
	b, err := debit(t, pool, true, "t6-leg-b", 2, "legacy1")
	if err != nil || b.IdempotentReplay || b.ChargedUSD != 2 || b.BalanceUSD != 3 {
		t.Fatalf("b=%+v err=%v", b, err)
	}
}

// A unique violation on a key this workspace cannot see (another tenant's row)
// must surface as an error, never as a free replay.
func TestDebitUniqueViolationNotVisibleIsError(t *testing.T) {
	pool := ledgerPool(t, "t6-uv-a", "t6-uv-b")
	credit(t, pool, "t6-uv-a", 5, "manual_grant", "")
	scoped, _ := debitKeys("t6-uv-a", "r1")
	insertRawEntry(t, pool, "t6-uv-b", scoped) // squats a's key in b's rows

	res, err := debit(t, pool, true, "t6-uv-a", 2, "r1")
	var ice *InsufficientCreditsError
	if err == nil || errors.As(err, &ice) || res.IdempotentReplay || res.ChargedUSD != 0 {
		t.Fatalf("res=%+v err=%v: want a plain error, not a replay", res, err)
	}
	if bal, rows := ledger(t, pool, "t6-uv-a"); bal != 5 || len(rows) != 1 {
		t.Fatalf("a wrote: bal=%v rows=%d", bal, len(rows))
	}
}
