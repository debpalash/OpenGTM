package pluginrun

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
)

// seedRuns inserts n runs for workspace ws with the given created_at values
// (so ties can be forced) and returns their ids in insertion order.
func seedRuns(t *testing.T, ws string, stamps []time.Time) []string {
	t.Helper()
	owner := dbtest.OwnerURL(t)
	pool := dbtest.Pool(t, dbtest.AppURL(t, owner), 2)
	ids := make([]string, 0, len(stamps))
	err := db.WithTenant(context.Background(), pool, ws, func(tx pgx.Tx) error {
		for i, at := range stamps {
			var id string
			if err := tx.QueryRow(context.Background(), `INSERT INTO plugin_runs
				(workspace_id, plugin_name, plugin_version, inputs, created_by, created_at)
				VALUES ($1, 'acme_team_page', '1.0.0', $2, 'seed', $3) RETURNING id::text`,
				ws, fmt.Sprintf(`{"n":%d}`, i), at).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func runIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["runs"].([]any)
	if !ok {
		t.Fatalf("no runs array in %+v", body)
	}
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)["id"].(string)
	}
	return out
}

func TestRunsKeysetPagination(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := dbtest.OwnerURL(t)
	// editor-a authorizes into ws-a; start from an empty ws-a and leave it empty.
	cleanup := dbtest.Pool(t, owner, 1)
	if _, err := cleanup.Exec(ctx, `DELETE FROM plugin_runs WHERE workspace_id = 'ws-a'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = cleanup.Exec(ctx, `DELETE FROM plugin_runs WHERE workspace_id IN ('ws-a','ws-b')`) })

	// 23 runs; every third shares its created_at with the previous one, so the
	// order depends on the id tie-break. Newest first is the reverse of the
	// (created_at, id) sort of the seeded rows.
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	stamps := make([]time.Time, 23)
	for i := range stamps {
		stamps[i] = base.Add(time.Duration(i-i%3) * time.Minute).Add(123456 * time.Microsecond)
	}
	ids := seedRuns(t, "ws-a", stamps)
	var want []string
	rows, err := cleanup.Query(ctx, `SELECT id::text FROM plugin_runs WHERE workspace_id = 'ws-a' ORDER BY created_at DESC, id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		want = append(want, id)
	}
	rows.Close()
	if len(want) != len(ids) {
		t.Fatalf("seeded %d, found %d", len(ids), len(want))
	}

	// Walk every page. A run created mid-walk must not disturb the pages that
	// follow (a newer row can only appear on a fresh first page).
	var got []string
	cursor, pages := "", 0
	for {
		path := "/api/v2/plugin-runs?limit=7"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		code, page := h.do("editor-a", "GET", path, nil)
		if code != http.StatusOK {
			t.Fatalf("page %d: %d %+v", pages, code, page)
		}
		pageIDs := runIDs(t, page)
		if len(pageIDs) > 7 {
			t.Fatalf("page %d has %d rows, limit 7", pages, len(pageIDs))
		}
		got = append(got, pageIDs...)
		pages++
		if pages == 1 {
			code, _ := h.do("editor-a", "POST", "/api/v2/plugin-runs",
				map[string]any{"plugin": "acme_team_page", "inputs": map[string]any{"domain": "late.example"}})
			if code != http.StatusAccepted {
				t.Fatalf("create mid-walk: %d", code)
			}
		}
		next, _ := page["next_cursor"].(string)
		if next == "" {
			if page["next_cursor"] != nil {
				t.Fatalf("last page next_cursor = %v, want null", page["next_cursor"])
			}
			break
		}
		if len(pageIDs) != 7 {
			t.Fatalf("a page with a next cursor must be full, got %d", len(pageIDs))
		}
		cursor = next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 4 { // 7+7+7+2 seeded rows
		t.Fatalf("walked %d pages, want 4", pages)
	}
	// The run created mid-walk has a newer created_at and is not in the walk.
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %s, want %s (duplicate, gap or wrong order)", i, got[i], want[i])
		}
	}

	// A fresh first page now starts with the late run.
	_, first := h.do("editor-a", "GET", "/api/v2/plugin-runs?limit=1", nil)
	if first["next_cursor"] == nil || runIDs(t, first)[0] == want[0] {
		t.Fatalf("a fresh first page should lead with the newest run: %+v", first)
	}

	// Exactly one page of rows: no next cursor.
	_, exact := h.do("editor-a", "GET", "/api/v2/plugin-runs?limit=200", nil)
	if exact["next_cursor"] != nil || len(runIDs(t, exact)) != len(want)+1 {
		t.Fatalf("limit 200: %d rows, cursor %v", len(runIDs(t, exact)), exact["next_cursor"])
	}

	// Garbage cursors are a 400, not a 500 or an empty page.
	enc := base64.RawURLEncoding.EncodeToString
	for _, bad := range []string{"!!!", enc([]byte("not-a-cursor")), enc([]byte("2026-10-01T12:00:00Z|not-a-uuid")),
		enc([]byte("yesterday|00000000-0000-0000-0000-000000000000"))} {
		if code, _ := h.do("editor-a", "GET", "/api/v2/plugin-runs?cursor="+bad, nil); code != http.StatusBadRequest {
			t.Fatalf("cursor %q: %d, want 400", bad, code)
		}
	}
}

func TestRunsCursorCannotCrossTenants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cleanup := dbtest.Pool(t, dbtest.OwnerURL(t), 1)
	if _, err := cleanup.Exec(ctx, `DELETE FROM plugin_runs WHERE workspace_id IN ('ws-a','ws-b')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = cleanup.Exec(ctx, `DELETE FROM plugin_runs WHERE workspace_id IN ('ws-a','ws-b')`) })

	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seedRuns(t, "ws-a", []time.Time{at, at.Add(time.Minute), at.Add(2 * time.Minute)})
	bIDs := seedRuns(t, "ws-b", []time.Time{at.Add(-time.Hour)})

	_, page := h.do("editor-a", "GET", "/api/v2/plugin-runs?limit=2", nil)
	cursor, _ := page["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("expected a cursor: %+v", page)
	}
	// Tenant B replaying A's cursor gets only B's rows older than that
	// position, never A's.
	_, b := h.do("editor-b", "GET", "/api/v2/plugin-runs?cursor="+url.QueryEscape(cursor), nil)
	got := runIDs(t, b)
	if len(got) != 1 || got[0] != bIDs[0] {
		t.Fatalf("tenant B saw %v, want only its own %v", got, bIDs)
	}
}
