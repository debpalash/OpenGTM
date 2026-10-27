package leadweb

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// GMaps is the client of the gosom/google-maps-scraper REST sidecar
// (legacy scrapers/gmaps_service_client.py): create a job, poll it, download
// the CSV. Plain REST, no browser work; the sidecar's host is the only
// internal address its egress client may reach. Every failure is an empty
// result (callers fall back to the browser-service Maps scraper).
type GMaps struct {
	base    string
	hc      *http.Client
	Poll    time.Duration // between status polls (default 5 s)
	Overall time.Duration // give up after (default 30 min)
	sleep   func(context.Context, time.Duration) error
}

// GMapsJob is the job request; zero values take the legacy defaults.
type GMapsJob struct {
	Lang    string // "en"
	Zoom    int    // 15
	Depth   int    // 1
	MaxTime int    // 3600 (seconds, sidecar-side)
}

// NewGMaps returns a client for GMAPS_SCRAPER_URL ("" = unavailable); hc
// nil builds the allow-listed egress client.
func NewGMaps(base string, hc *http.Client) *GMaps {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if hc == nil && base != "" {
		hc = egress.NewOperatorClient(allowHostsFor(base))
	}
	return &GMaps{base: base, hc: hc, Poll: 5 * time.Second, Overall: 30 * time.Minute, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Available reports whether the sidecar is configured.
func (g *GMaps) Available() bool { return g != nil && g.base != "" }

// SearchPlaces runs one scrape job to completion and returns its CSV rows
// (column name -> value), or nil.
func (g *GMaps) SearchPlaces(ctx context.Context, keywords []string, job GMapsJob) []map[string]string {
	if !g.Available() {
		return nil
	}
	if job.Lang == "" {
		job.Lang = "en"
	}
	if job.Zoom == 0 {
		job.Zoom = 15
	}
	if job.Depth == 0 {
		job.Depth = 1
	}
	if job.MaxTime == 0 {
		job.MaxTime = 3600
	}
	name := truncRunes(strings.Join(keywords, " / "), 120)
	if name == "" {
		name = "leadgen job"
	}
	if keywords == nil {
		keywords = []string{}
	}
	body := struct {
		Name     string   `json:"name"`
		Keywords []string `json:"keywords"`
		Lang     string   `json:"lang"`
		Zoom     int      `json:"zoom"`
		Depth    int      `json:"depth"`
		MaxTime  int      `json:"max_time"`
	}{name, keywords, job.Lang, job.Zoom, job.Depth, job.MaxTime}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(body) != nil {
		return nil
	}
	status, data := g.do(ctx, http.MethodPost, "/api/v1/jobs", bytes.TrimRight(buf.Bytes(), "\n"))
	if status != http.StatusOK && status != http.StatusCreated {
		return nil
	}
	var created struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &created) != nil || created.ID == "" {
		return nil
	}
	state := "pending"
	for waited := time.Duration(0); waited < g.Overall; {
		if g.sleep(ctx, g.Poll) != nil {
			return nil
		}
		waited += g.Poll
		st, d := g.do(ctx, http.MethodGet, "/api/v1/jobs/"+created.ID, nil)
		if st != http.StatusOK {
			continue
		}
		var js struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(d, &js)
		state = js.Status
		if state == "ok" || state == "failed" {
			break
		}
	}
	if state != "ok" {
		return nil
	}
	st, csvData := g.do(ctx, http.MethodGet, "/api/v1/jobs/"+created.ID+"/download", nil)
	if st != http.StatusOK || strings.TrimSpace(string(csvData)) == "" {
		return nil
	}
	return csvRows(csvData)
}

// csvRows is csv.DictReader: the first row names the columns; short rows
// leave the missing columns empty.
func csvRows(data []byte) []map[string]string {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	recs, err := r.ReadAll()
	if err != nil || len(recs) == 0 {
		return nil
	}
	cols := recs[0]
	out := make([]map[string]string, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		row := make(map[string]string, len(cols))
		for i, c := range cols {
			if i < len(rec) {
				row[c] = rec[i]
			} else {
				row[c] = ""
			}
		}
		out = append(out, row)
	}
	return out
}

func (g *GMaps) do(ctx context.Context, method, path string, body []byte) (int, []byte) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rdr)
	if err != nil {
		return 0, nil
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, nil
	}
	return resp.StatusCode, data
}
