package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

const (
	historyMaxLimit = 1000
	maxScrapeEmails = 500
	maxEmailChars   = 320
)

// scrapeStore persists POST /api/v2/scrape results into the legacy global tables scrape_history, links and
// email_data (no workspace column, no RLS: the admin scraper is a global tool).
type scrapeStore interface {
	Save(ctx context.Context, requestURL string, res scrapeResult) error
	List(ctx context.Context, skip, limit int) ([]historyRow, error)
}

type historyRow struct {
	ID         int32   `json:"id"`
	URL        *string `json:"url"`
	Title      *string `json:"title"`
	Method     *string `json:"method"`
	WordCount  *int32  `json:"word_count"`
	EmailCount *int32  `json:"email_count"`
	CreatedAt  *string `json:"created_at"`
}

type pgScrapeStore struct{ pool *pgxpool.Pool }

// Save writes the history row, the link (once per requested URL) and one email_data row per new address, in one
// transaction (Python committed each step on its own).
func (s pgScrapeStore) Save(ctx context.Context, requestURL string, res scrapeResult) error {
	now := time.Now().UTC()
	emails := res.Emails
	if len(emails) > maxScrapeEmails {
		emails = emails[:maxScrapeEmails]
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO scrape_history (url, title, method, word_count, email_count, created_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			res.URL, res.Title, res.Method, res.WordCount, len(res.Emails), now.Format("2006-01-02 15:04:05")); err != nil {
			return err
		}
		var linkID int32
		err := tx.QueryRow(ctx, `SELECT id FROM links WHERE url = $1`, requestURL).Scan(&linkID)
		if err == pgx.ErrNoRows {
			err = tx.QueryRow(ctx, `INSERT INTO links (url, status, created_at, source) VALUES ($1,'Completed',$2,'Scraper V2')
				ON CONFLICT (url) DO UPDATE SET url = EXCLUDED.url RETURNING id`, requestURL, now.Format("2006-01-02T15:04:05.000000+00:00")).Scan(&linkID)
		}
		if err != nil {
			return err
		}
		for _, e := range emails {
			if len(e) > maxEmailChars {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO email_data (name, email, source_link_id, created_at, is_used)
				SELECT '', $1::varchar, $2, $3, false WHERE NOT EXISTS (SELECT 1 FROM email_data WHERE email = $1::varchar AND source_link_id = $2)`,
				e, linkID, now.Format("2006-01-02T15:04:05.000000+00:00")); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s pgScrapeStore) List(ctx context.Context, skip, limit int) ([]historyRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, url, title, method, word_count, email_count, created_at FROM scrape_history
		ORDER BY id DESC OFFSET $1 LIMIT $2`, skip, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []historyRow{}
	for rows.Next() {
		var h historyRow
		if err := rows.Scan(&h.ID, &h.URL, &h.Title, &h.Method, &h.WordCount, &h.EmailCount, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// history is routers/scraper.py get_scrape_history: newest first. Negative skip counts as 0 and limit is clamped to
// [0, 1000] (Postgres rejects a negative OFFSET or LIMIT, which Python answered with a 500).
func (h *scrapeRoutes) history(w http.ResponseWriter, r *http.Request) {
	skip, ok := queryInt(w, r, "skip", 0, nil, nil)
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit", 50, nil, nil)
	if !ok {
		return
	}
	skip = max(skip, 0)
	limit = min(max(limit, 0), historyMaxLimit)
	rows, err := h.store.List(r.Context(), skip, limit)
	if err != nil {
		h.log.Error("v2/scrapes: list failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writePy(w, rows)
}
