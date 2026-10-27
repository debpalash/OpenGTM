//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// TestAgentVectors replays the agent cells recorded from legacy run_agent_cell
// (internal/aicol/testdata/record.py, section 7) through the native Go agent: provider
// calls in order, cell value/provider/error and the reasoning trace (goal, steps, outcome, cost).
func TestAgentVectors(t *testing.T) {
	raw, err := os.ReadFile("../aicol/testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Agent []struct {
			Name      string
			Column    map[string]any
			Costs     map[string]float64
			Providers map[string]struct {
				Fields  map[string]any
				Success *bool
				Raise   string
				Timeout bool
			}
			Calls  []string
			Result struct {
				Value, Provider, Error *string
			}
			Trace struct {
				Goal         string
				Steps        []any
				Outcome      string
				TotalCostUSD any `json:"total_cost_usd"`
			}
		}
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	e := setup(t, wsA)
	for _, sc := range v.Agent {
		t.Run(sc.Name, func(t *testing.T) {
			e.t = t
			e.inWS(wsA, func(tx pgx.Tx) error { _, err := tx.Exec(e.ctx, `DELETE FROM cell_traces`); return err })
			wb := "wb-agv-" + sc.Name
			cols, _ := json.Marshal([]any{sc.Column})
			e.seed(wsA, wb, string(cols), 1, 10)
			costs := map[string]float64{}
			for name := range sc.Providers {
				costs[name] = 0
			}
			for _, s := range sc.Trace.Steps {
				costs[s.(map[string]any)["provider"].(string)] = 0
			}
			for k, c := range sc.Costs {
				costs[k] = c
			}
			fr := &fakeRunner{fn: func(_ context.Context, p string, _ providers.Input) (providers.Output, error) {
				r, ok := sc.Providers[p]
				switch {
				case !ok:
					return providers.Output{}, provcall.ErrUnknownProvider
				case r.Timeout:
					return providers.Output{Error: "timeout"}, nil
				case r.Raise != "":
					return providers.Output{}, errors.New(r.Raise)
				}
				out := providers.Output{Confidence: 0.9}
				if r.Success != nil && !*r.Success {
					out.Error = "failed"
				}
				if len(r.Fields) > 0 {
					out.Extra = providers.NewObj()
					for k, x := range r.Fields {
						out.Extra.Set(k, x)
					}
				}
				return out, nil
			}}
			d := nativeDeps(e, &fakeLLM{})
			d.Runner = fr
			if _, err := workbook.Run(e.ctx, d, e.claim(wsA, payloadNoManifests(wsA, wb, costs, nil))); err != nil {
				t.Fatal(err)
			}
			// The runner answers ErrUnknownProvider for an unregistered provider without any I/O
			// (Python asked get_provider first); only real provider calls are compared.
			var called []string
			for _, c := range fr.calls {
				if _, ok := sc.Providers[c]; ok {
					called = append(called, c)
				}
			}
			if strings.Join(called, ",") != strings.Join(sc.Calls, ",") {
				t.Fatalf("calls %v want %v", called, sc.Calls)
			}
			want := func(p *string) string {
				if p == nil {
					return ""
				}
				return *p
			}
			if got := overlay(e, wb, "ag", "value"); got != want(sc.Result.Value) {
				t.Fatalf("value %q want %q", got, want(sc.Result.Value))
			}
			if got := overlay(e, wb, "ag", "error"); got != want(sc.Result.Error) {
				t.Fatalf("error %q want %q", got, want(sc.Result.Error))
			}
			if sc.Result.Provider != nil {
				if got := overlay(e, wb, "ag", "provider"); got != *sc.Result.Provider {
					t.Fatalf("provider %q", got)
				}
			}
			var goal, outcome, steps, cost string
			e.inWS(wsA, func(tx pgx.Tx) error {
				return tx.QueryRow(e.ctx, `SELECT goal, outcome, steps::text, total_cost_usd::text FROM cell_traces WHERE workbook_id=$1`, wb).
					Scan(&goal, &outcome, &steps, &cost)
			})
			var gotSteps, gotCost any
			_ = json.Unmarshal([]byte(steps), &gotSteps)
			_ = json.Unmarshal([]byte(cost), &gotCost)
			wantSteps := any(sc.Trace.Steps)
			if sc.Trace.Steps == nil {
				wantSteps = []any{}
			}
			if goal != sc.Trace.Goal || outcome != sc.Trace.Outcome || !reflect.DeepEqual(gotSteps, wantSteps) ||
				!reflect.DeepEqual(gotCost, sc.Trace.TotalCostUSD) {
				t.Fatalf("trace %q %q %s %s\nwant %q %q %v %v", goal, outcome, steps, cost, sc.Trace.Goal, sc.Trace.Outcome, sc.Trace.Steps, sc.Trace.TotalCostUSD)
			}
		})
	}
}
