//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

type fakeEmitter struct {
	mu    sync.Mutex
	calls []emitCall
}

type emitCall struct {
	ws, wb  string
	changes []automationhook.Change
}

func (f *fakeEmitter) RowsChanged(_ context.Context, tx pgx.Tx, ws, wb string, ch []automationhook.Change) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, emitCall{ws, wb, ch})
	return len(ch), nil
}

// F4: a run emits one RowsChanged per workbook at run end, only for complete cells whose
// prior overlay value differs (Python str() compare), old values decoded with UseNumber.
func TestRunEndFlushesRowsChanged(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-rowchg", "["+colJSON("c1", "p1")+"]", 3, 10)
	e.inWS(wsA, func(tx pgx.Tx) error {
		// row0: big integer (float64 would round it); row1: already the new value; row2: no prior cell.
		for i, cell := range []string{`{"value": 9007199254740993, "status": "complete"}`, `{"value": "1999", "status": "complete"}`} {
			if _, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET enrichments = jsonb_build_object('c1', $1::jsonb)::json WHERE id = $2`, cell, ids[i]); err != nil {
				return err
			}
		}
		return nil
	})
	s := newSim(t, nil)
	c := e.claim(wsA, payloadFor(wsA, "wb-rowchg", map[string]float64{"p1": 0.05}, nil))
	deps := e.deps(s, nil)
	em := &fakeEmitter{}
	deps.Emitter = em
	if _, err := workbook.Run(e.ctx, deps, c); err != nil {
		t.Fatal(err)
	}
	if len(em.calls) != 1 || em.calls[0].ws != wsA || em.calls[0].wb != "wb-rowchg" {
		t.Fatalf("calls %+v", em.calls)
	}
	got := map[int64]automationhook.Change{}
	for _, ch := range em.calls[0].changes {
		got[ch.RowID] = ch
	}
	if len(got) != 2 {
		t.Fatalf("changes %+v", em.calls[0].changes)
	}
	big := got[int64(ids[0])]
	if n, ok := big.Old.(json.Number); !ok || n.String() != "9007199254740993" || big.Field != "c1" || big.New != "1999" {
		t.Fatalf("old not decoded with UseNumber: %#v", big)
	}
	if o := got[int64(ids[2])]; o.Old != nil || o.New != "1999" {
		t.Fatalf("new cell change %#v", o)
	}
	if _, dup := got[int64(ids[1])]; dup {
		t.Fatal("unchanged value emitted")
	}
	if big.CellVersion == "" {
		t.Fatal("no cell_version")
	}
}
