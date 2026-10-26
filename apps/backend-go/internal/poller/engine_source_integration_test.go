//go:build integration

package poller

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Python _poll_one_source vectors (poll_source_vectors.json, recorded from the
// real engine with the detectors stubbed) against pollOneSource: result tuple,
// emitted signal rows (deterministic ids), merged cursor, resolved_cik and
// last_error.
func TestPollSourceVectors(t *testing.T) {
	const ws, wid, leadOff = "ws-1", "w-1", 8_000_000
	pool := testpg.NewAppPool(t, 10)
	ctx := context.Background()
	clean := func() {
		_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			for _, q := range []string{`DELETE FROM watch_subscriptions`, `DELETE FROM signals`, `DELETE FROM leads`,
				`DELETE FROM credit_ledger_entries`, `DELETE FROM workspace_credits`} {
				_, _ = tx.Exec(ctx, q)
			}
			return nil
		})
	}
	clean()
	t.Cleanup(clean)

	raw, err := os.ReadFile("testdata/poll_source_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	type jev struct {
		Natural     string  `json:"natural_event_id"`
		SignalType  string  `json:"signal_type"`
		Title       string  `json:"title"`
		Description string  `json:"description"`
		Source      string  `json:"source"`
		SourceURL   string  `json:"source_url"`
		Weight      float64 `json:"weight"`
		OccurredAt  float64 `json:"occurred_at"`
		LeadID      *int64  `json:"lead_id"`
		Company     string  `json:"company"`
	}
	var file struct {
		Vectors []struct {
			Name  string `json:"name"`
			Src   string `json:"src"`
			Watch struct {
				Kind        string         `json:"kind"`
				Target      string         `json:"target"`
				SignalTypes []string       `json:"signal_types"`
				Config      map[string]any `json:"config"`
				Cursor      map[string]any `json:"cursor"`
				ResolvedCIK *string        `json:"resolved_cik"`
			} `json:"watch"`
			LeadID         *int64             `json:"lead_id"`
			DetectorResult json.RawMessage    `json:"detector_result"`
			Balance        float64            `json:"balance"`
			Costs          map[string]float64 `json:"costs"`
			Backfill       bool               `json:"backfill"`
			Result         json.RawMessage    `json:"result"`
			StubDebit      bool               `json:"stub_debit"`
			Debits         []struct {
				Cost  float64 `json:"cost"`
				RunID string  `json:"run_id"`
			} `json:"debits"`
			WebTechCalls []struct {
				Website string `json:"website"`
			} `json:"web_tech_calls"`
			Signals []struct {
				ID          string  `json:"id"`
				Company     string  `json:"company"`
				SignalType  string  `json:"signal_type"`
				Title       string  `json:"title"`
				Description string  `json:"description"`
				Source      string  `json:"source"`
				LeadID      int     `json:"lead_id"`
				Weight      int     `json:"weight"`
				SourceURL   string  `json:"source_url"`
				CreatedAt   float64 `json:"created_at"`
			} `json:"signals"`
			WatchAfter struct {
				Fails       int            `json:"consecutive_failures"`
				LastError   *string        `json:"last_error"`
				Cursor      map[string]any `json:"cursor"`
				ResolvedCIK *string        `json:"resolved_cik"`
			} `json:"watch_after"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) != 28 {
		t.Fatalf("vectors = %d", len(file.Vectors))
	}
	for _, v := range file.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			clean()
			types, _ := json.Marshal(v.Watch.SignalTypes)
			cfgJSON, cursorJSON := []byte("{}"), []byte(`{"bootstrapped": false}`)
			if v.Watch.Config != nil {
				cfgJSON, _ = json.Marshal(v.Watch.Config)
			}
			if v.Watch.Cursor != nil {
				cursorJSON, _ = json.Marshal(v.Watch.Cursor)
			}
			var lead int64
			if v.LeadID != nil {
				lead = leadOff + *v.LeadID
			}
			if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, resolved_cik, signal_types, "interval",
					config, enabled, cursor, consecutive_failures) VALUES ($1,$2,$3,$4,$5,$6::json,'daily',$7::json,true,$8::json,0)`,
					wid, ws, v.Watch.Kind, v.Watch.Target, v.Watch.ResolvedCIK, string(types), string(cfgJSON), string(cursorJSON)); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO leads (id, workspace_id, company, website) VALUES ($1,$2,'Hot Pursuit Labs','https://hotpursuit.example.com')`, leadOff+14, ws); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO workspace_credits (workspace_id, balance_usd) VALUES ($1,$2)`, ws, v.Balance)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			// The recorded detector result has its side-channel keys already popped by
			// Python; rebuild them from the recorded outcome.
			var det []json.RawMessage
			detNone := string(v.DetectorResult) == "null"
			if !detNone {
				if err := json.Unmarshal(v.DetectorResult, &det); err != nil {
					t.Fatal(err)
				}
			}
			var evs []DetectedEvent
			var patch map[string]any
			if !detNone {
				var jevs []jev
				if string(det[0]) != "null" {
					if err := json.Unmarshal(det[0], &jevs); err != nil {
						t.Fatal(err)
					}
				}
				evs = []DetectedEvent{}
				for _, e := range jevs {
					de := DetectedEvent{SignalType: e.SignalType, NaturalEventID: e.Natural, Title: e.Title, Description: e.Description,
						Source: e.Source, SourceURL: e.SourceURL, Weight: e.Weight, OccurredAt: e.OccurredAt, Company: e.Company}
					if e.LeadID != nil {
						l := leadOff + *e.LeadID
						de.LeadID = &l
					}
					evs = append(evs, de)
				}
				if string(det[1]) != "null" {
					dec := json.NewDecoder(strings.NewReader(string(det[1])))
					dec.UseNumber()
					if err := dec.Decode(&patch); err != nil {
						t.Fatal(err)
					}
					for k, x := range patch {
						if n, ok := x.(json.Number); ok {
							f, _ := n.Float64()
							patch[k] = f
						}
					}
				}
			}
			if rc := v.WatchAfter.ResolvedCIK; rc != nil && (v.Watch.ResolvedCIK == nil || *v.Watch.ResolvedCIK != *rc) {
				patch["_resolved_cik"] = *rc
			}
			var res []any
			if string(v.Result) != "null" && string(v.Result) != "false" {
				_ = json.Unmarshal(v.Result, &res)
			}
			if len(res) == 3 && res[2] == true {
				msg := *v.WatchAfter.LastError
				if len(msg) == 255 {
					msg += strings.Repeat("f", 45) // the recorded text was cut at 255
				}
				patch["_collector_failures"] = []any{msg}
			}

			var gotWebsite []string
			d := HandlerDeps{}
			d.Cfg = Config{FundingCostUSD: v.Costs["funding"], HiringCostUSD: v.Costs["hiring"], FeedCostUSD: v.Costs["feed"], BillingEnabled: true}
			d.Now = func() time.Time { return time.Unix(1_800_000_000, 0) }
			d.Fetch = func(_ context.Context, src string, w PyWatch, l int64, backfill bool) ([]DetectedEvent, map[string]any, bool, error) {
				if src == "web_tech" {
					gotWebsite = append(gotWebsite, "called")
				}
				if src == "bogus" {
					return nil, nil, false, nil
				}
				if backfill != v.Backfill {
					t.Errorf("backfill = %v", backfill)
				}
				if detNone {
					return nil, nil, true, errors.New("detector failed")
				}
				return evs, patch, true, nil
			}
			var out sourceOutcome
			err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				var err error
				out, err = d.pollOneSource(ctx, tx, wid, ws, v.Src, "fk-1", lead)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}

			// result
			switch string(v.Result) {
			case "false":
				if !out.failed {
					t.Errorf("want failure, got %+v", out)
				}
			case "null":
				// Python crashed (a real debit inside begin) or the source is unknown. Go charges
				// and emits (documented deviation); an unknown source is a no-op.
				if v.Src == "bogus" && (out.failed || out.inserted != 0) {
					t.Errorf("unknown source = %+v", out)
				}
				if v.Name == "billing_real_check_and_debit_inside_begin_raises_in_python" && (out.failed || out.inserted != 1) {
					t.Errorf("go outcome = %+v", out)
				}
			default:
				if out.inserted != int(res[0].(float64)) || out.dupe != int(res[1].(float64)) || out.failed != res[2].(bool) {
					t.Errorf("outcome = %+v, want %v", out, res)
				}
			}
			if v.Src == "web_tech" && len(v.WebTechCalls) == 1 && len(gotWebsite) != 1 {
				t.Errorf("web_tech not fetched")
			}

			// signals
			type row struct {
				ID, Company, Type, Title, Desc, Source, URL string
				Lead, Weight                                int
				Created                                     float64
			}
			var got []row
			var cursor []byte
			var lastErr, rc *string
			var bal float64
			var ledgerN int
			if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT id, company, signal_type, title, description, source, source_url, lead_id, weight, created_at FROM signals ORDER BY created_at, id`)
				if err != nil {
					return err
				}
				for rows.Next() {
					var r row
					if err := rows.Scan(&r.ID, &r.Company, &r.Type, &r.Title, &r.Desc, &r.Source, &r.URL, &r.Lead, &r.Weight, &r.Created); err != nil {
						rows.Close()
						return err
					}
					got = append(got, r)
				}
				rows.Close()
				if err := tx.QueryRow(ctx, `SELECT cursor::text::bytea, last_error, resolved_cik FROM watch_subscriptions WHERE id=$1`, wid).Scan(&cursor, &lastErr, &rc); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, `SELECT balance_usd FROM workspace_credits WHERE workspace_id=$1`, ws).Scan(&bal); err != nil {
					return err
				}
				return tx.QueryRow(ctx, `SELECT count(*) FROM credit_ledger_entries WHERE workspace_id=$1 AND reason='poller_fetch'`, ws).Scan(&ledgerN)
			}); err != nil {
				t.Fatal(err)
			}
			if string(v.Result) != "null" || v.Name == "billing_real_check_and_debit_inside_begin_raises_in_python" {
				var want []row
				for _, s := range v.Signals {
					l := s.LeadID
					if l != 0 {
						l += leadOff
					}
					want = append(want, row{s.ID, s.Company, s.SignalType, s.Title, s.Description, s.Source, s.SourceURL, l, s.Weight, s.CreatedAt})
				}
				if v.Name == "billing_real_check_and_debit_inside_begin_raises_in_python" {
					want = got // Python raised before emitting; Go emits (deviation), asserted via out above
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("signals:\n got %+v\nwant %+v", got, want)
				}
				var gotCursor map[string]any
				_ = json.Unmarshal(cursor, &gotCursor)
				wantCursor := v.WatchAfter.Cursor
				if !reflect.DeepEqual(gotCursor, wantCursor) && v.Name != "billing_real_check_and_debit_inside_begin_raises_in_python" {
					t.Errorf("cursor = %v, want %v", gotCursor, wantCursor)
				}
				if !reflect.DeepEqual(lastErr, v.WatchAfter.LastError) || !reflect.DeepEqual(rc, v.WatchAfter.ResolvedCIK) {
					if v.Name != "billing_real_check_and_debit_inside_begin_raises_in_python" {
						t.Errorf("last_error=%v resolved_cik=%v, want %v %v", lastErr, rc, v.WatchAfter.LastError, v.WatchAfter.ResolvedCIK)
					}
				}
			}
			// billing: Go debits for real where Python stubbed (or crashed): one charge per
			// accepted debit, none when refused.
			wantCharge := 0.0
			if string(v.Result) != "false" || v.WatchAfter.LastError == nil || *v.WatchAfter.LastError != "insufficient_credits" {
				seen := map[string]bool{}
				for _, db := range v.Debits {
					if !seen[db.RunID] {
						seen[db.RunID] = true
						wantCharge += db.Cost
					}
				}
			}
			if v.Name == "billing_real_check_and_debit_inside_begin_raises_in_python" {
				wantCharge = 0.05
			}
			if math.Abs((v.Balance-bal)-wantCharge) > 1e-9 {
				t.Errorf("charged %.4f, want %.4f", v.Balance-bal, wantCharge)
			}
			if wantCharge > 0 && ledgerN != 1 {
				t.Errorf("ledger entries = %d", ledgerN)
			}
		})
	}
}
