//go:build integration

package refresh_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsA = "ws-p06t16-a"
	wsB = "ws-p06t16-b"
)

func setupPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(ctx, `DELETE FROM workbook_activity`)
				_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE workspace_id = $1`, w)
				_, err := tx.Exec(ctx, `DELETE FROM workbooks`)
				return err
			})
		}
	}
	clean()
	t.Cleanup(clean)
	refresh.Clock = func() time.Time { return fixedNow }
	t.Cleanup(func() { refresh.Clock = time.Now })
	return pool, ctx
}

func mkWorkbook(t *testing.T, ctx context.Context, tx pgx.Tx, ws, id, cols, policy string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id, columns_config, refresh_policy)
		VALUES ($1, $1, $2, $3::json, $4::json)`, id, ws, cols, policy); err != nil {
		t.Fatal(err)
	}
}

func mkRow(t *testing.T, ctx context.Context, tx pgx.Tx, ws, wb string, id int, lead *int) {
	t.Helper()
	if _, err := tx.Exec(ctx, `INSERT INTO workbook_rows (id, workbook_id, workspace_id, lead_id, data)
		VALUES ($1, $2, $3, $4, '{}'::json)`, id, wb, ws, lead); err != nil {
		t.Fatal(err)
	}
}

func mkOverlay(t *testing.T, ctx context.Context, tx pgx.Tx, ws, wb string, lead int, col string, status *string, updated *time.Time) {
	t.Helper()
	var u any
	if updated != nil {
		u = updated.UTC().Format("2006-01-02 15:04:05.999999")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workbook_enrichments (workbook_id, workspace_id, lead_id, column_id, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::timestamp)`, wb, ws, lead, col, status, u); err != nil {
		t.Fatal(err)
	}
}

type staleCase struct {
	Name string `json:"name"`
	Rows []struct {
		ID         int    `json:"id"`
		LeadID     *int   `json:"lead_id"`
		WorkbookID string `json:"workbook_id"`
	} `json:"rows"`
	Overlays []struct {
		LeadID         int      `json:"lead_id"`
		ColumnID       string   `json:"column_id"`
		Status         *string  `json:"status"`
		UpdatedDaysAgo *float64 `json:"updated_days_ago"`
		WorkbookID     string   `json:"workbook_id"`
	} `json:"overlays"`
	Columns []map[string]any    `json:"columns"`
	TTLMap  map[string]float64  `json:"ttl_map"`
	Now     string              `json:"now"`
	Result  map[string][]string `json:"result"`
}

func TestStaleRowColumnsVectors(t *testing.T) {
	pool, ctx := setupPool(t)
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Cases []staleCase `json:"stale_row_columns"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Cases) < 20 {
		t.Fatalf("expected the Python vector set, got %d", len(d.Cases))
	}
	for _, c := range d.Cases {
		now, err := time.Parse(time.RFC3339, c.Now)
		if err != nil {
			t.Fatal(err)
		}
		var cols []refresh.Col
		for _, m := range c.Columns {
			id, _ := m["id"].(string)
			tf, _ := m["target_field"].(string)
			cols = append(cols, refresh.Col{ID: id, TargetField: tf})
		}
		for _, batch := range []int{1, 2, 1000} {
			t.Run(fmt.Sprintf("%s/batch%d", c.Name, batch), func(t *testing.T) {
				var got map[int64][]string
				err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
					defer func() { _, _ = tx.Exec(ctx, `DELETE FROM workbooks`) }()
					mkWorkbook(t, ctx, tx, wsA, "wb1", `[]`, `{}`)
					mkWorkbook(t, ctx, tx, wsA, "wb2", `[]`, `{}`)
					for _, r := range c.Rows {
						mkRow(t, ctx, tx, wsA, r.WorkbookID, r.ID, r.LeadID)
					}
					for _, o := range c.Overlays {
						var u *time.Time
						if o.UpdatedDaysAgo != nil {
							x := now.Add(-time.Duration(*o.UpdatedDaysAgo * 24 * float64(time.Hour)))
							u = &x
						}
						mkOverlay(t, ctx, tx, wsA, o.WorkbookID, o.LeadID, o.ColumnID, o.Status, u)
					}
					var e error
					got, e = refresh.StaleRowColumnsBatch(ctx, tx, "wb1", cols, c.TTLMap, now, batch)
					// force rollback so the case leaves nothing behind
					if e != nil {
						return e
					}
					return errRollback
				})
				if err != errRollback && err != nil {
					t.Fatal(err)
				}
				want := map[int64][]string{}
				for k, v := range c.Result {
					var id int64
					fmt.Sscan(k, &id)
					want[id] = v
				}
				if len(got) == 0 {
					got = map[int64][]string{}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got %v want %v", got, want)
				}
			})
		}
	}
}

var errRollback = fmt.Errorf("rollback")
