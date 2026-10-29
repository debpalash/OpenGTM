//go:build integration

package provaccuracy

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPersistMatchesPython(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var v struct {
		Before []struct {
			Provider string `json:"provider"`
			Field    string `json:"field"`
			Attempts int    `json:"attempts"`
			Hits     int    `json:"hits"`
		} `json:"before"`
		Today string `json:"today"`
		Rows  []struct {
			Provider string   `json:"provider"`
			Field    string   `json:"field"`
			Score    *float64 `json:"score"`
			Samples  int      `json:"samples"`
			Attempts int      `json:"attempts"`
			Hits     int      `json:"hits"`
			HasUpd   bool     `json:"has_updated"`
		} `json:"rows"`
	}
	b, _ := os.ReadFile("testdata/persist.json")
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM provider_stats`); err != nil {
		t.Fatal(err)
	}
	for _, r := range v.Before {
		if _, err := tx.Exec(ctx, `INSERT INTO provider_stats (provider, field, attempts, hits, accuracy_samples) VALUES ($1,$2,$3,$4,0)`, r.Provider, r.Field, r.Attempts, r.Hits); err != nil {
			t.Fatal(err)
		}
	}
	ranking, err := RunEval(day(t, v.Today))
	if err != nil {
		t.Fatal(err)
	}
	golden, outs, _ := Dataset()
	if _, err := Persist(ctx, tx, ScoreOutputs(golden, outs, day(t, v.Today)), timeNow()); err != nil {
		t.Fatal(err)
	}
	_ = ranking
	rows, err := tx.Query(ctx, `SELECT provider, field, accuracy_score, accuracy_samples, attempts, hits, accuracy_updated_at IS NOT NULL FROM provider_stats ORDER BY provider, field`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var p, f string
		var score *float64
		var samples, attempts, hits int32
		var upd bool
		if err := rows.Scan(&p, &f, &score, &samples, &attempts, &hits, &upd); err != nil {
			t.Fatal(err)
		}
		if i >= len(v.Rows) {
			t.Fatalf("extra row %s/%s", p, f)
		}
		w := v.Rows[i]
		i++
		if p != w.Provider || f != w.Field || int(samples) != w.Samples || int(attempts) != w.Attempts || int(hits) != w.Hits || upd != w.HasUpd {
			t.Errorf("row %s/%s = samples %d attempts %d hits %d upd %v; want %+v", p, f, samples, attempts, hits, upd, w)
		}
		if (score == nil) != (w.Score == nil) || (score != nil && *score != *w.Score) {
			t.Errorf("row %s/%s score = %v, want %v", p, f, score, w.Score)
		}
	}
	if i != len(v.Rows) {
		t.Errorf("%d rows, want %d", i, len(v.Rows))
	}
}

func timeNow() time.Time { return time.Now() }
