package personintel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// nowUTC is a var for tests.
var nowUTC = func() time.Time { return time.Now().UTC() }

func txt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// Save inserts the profile (global table person_intel; created_at is naive
// UTC like Python's datetime.now(timezone.utc) into a timestamp column) and
// returns the new id. The JSON columns keep Python's collection shapes.
func Save(ctx context.Context, tx pgx.Tx, p Profile) (int64, error) {
	cols := map[string]any{
		"emails": nilToEmpty(p.Emails), "articles": nilArr(p.Articles), "mentions": nilArr(p.Mentions),
		"companies": nilToEmpty(p.Companies), "education": nilToEmpty(p.Education),
		"skills": nilToEmpty(p.Skills), "raw_sources": nilArr(p.RawSources),
	}
	enc := map[string][]byte{}
	for k, v := range cols {
		b, err := encodeNoEscape(v)
		if err != nil {
			return 0, err
		}
		enc[k] = b
	}
	social, err := encodeNoEscape(p.social())
	if err != nil {
		return 0, err
	}
	id, err := gen.New(tx).PersonInsert(ctx, gen.PersonInsertParams{
		LinkedinUrl: txt(p.LinkedInURL), Username: txt(p.Username), Name: txt(p.Name),
		Headline: txt(p.Headline), Location: txt(p.Location), Summary: txt(p.Summary),
		Emails: enc["emails"], SocialLinks: social, Articles: enc["articles"], Mentions: enc["mentions"],
		Companies: enc["companies"], Education: enc["education"], Skills: enc["skills"],
		RawSources: enc["raw_sources"], Status: txt(p.Status),
		CreatedAt: pgtype.Timestamp{Time: nowUTC().Truncate(time.Microsecond), Valid: true},
	})
	return int64(id), err
}

func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nilArr[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// isoformat renders a naive timestamp like Python's datetime.isoformat() ("" when null).
func isoformat(t pgtype.Timestamp) string {
	if !t.Valid {
		return ""
	}
	if t.Time.Nanosecond()/1000 == 0 {
		return t.Time.Format("2006-01-02T15:04:05")
	}
	return t.Time.Format("2006-01-02T15:04:05.000000")
}

func strPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// orEmpty is Python's `x or ""`.
func orEmpty(t pgtype.Text) string { return t.String }

// HistoryRow is one /api/person/history entry (field order is the JSON key order).
type HistoryRow struct {
	ID          int64   `json:"id"`
	LinkedInURL *string `json:"linkedin_url"`
	Username    *string `json:"username"`
	Name        string  `json:"name"`
	Headline    string  `json:"headline"`
	EmailCount  int     `json:"email_count"`
	Status      *string `json:"status"`
	CreatedAt   string  `json:"created_at"`
}

func clamp32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}

// History lists records newest first (id DESC). A negative skip or limit is
// rejected by Postgres, as in Python (500).
func History(ctx context.Context, tx pgx.Tx, skip, limit int) ([]HistoryRow, error) {
	rows, err := gen.New(tx).PersonHistory(ctx, gen.PersonHistoryParams{RowSkip: clamp32(skip), RowLimit: clamp32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]HistoryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, HistoryRow{
			ID: int64(r.ID), LinkedInURL: strPtr(r.LinkedinUrl), Username: strPtr(r.Username),
			Name: orEmpty(r.Name), Headline: orEmpty(r.Headline), EmailCount: jsonLen(r.Emails),
			Status: strPtr(r.Status), CreatedAt: isoformat(r.CreatedAt),
		})
	}
	return out, nil
}

func jsonLen(raw []byte) int {
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return 0
	}
	return len(a)
}

// Detail is a full record (GET /api/person/{id}). Marshal it with JSON().
type Detail struct {
	ID          int64
	LinkedInURL *string
	Username    *string
	Name        *string
	Headline    *string
	Location    *string
	Summary     *string
	Emails      []byte
	SocialLinks []byte
	Articles    []byte
	Mentions    []byte
	Companies   []byte
	Education   []byte
	Skills      []byte
	RawSources  []byte
	Status      *string
	CreatedAt   string
}

// Get returns the record, or nil when it does not exist.
func Get(ctx context.Context, tx pgx.Tx, id int64) (*Detail, error) {
	if id > math.MaxInt32 || id < math.MinInt32 {
		return nil, nil
	}
	r, err := gen.New(tx).PersonGet(ctx, int32(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Detail{
		ID: int64(r.ID), LinkedInURL: strPtr(r.LinkedinUrl), Username: strPtr(r.Username),
		Name: strPtr(r.Name), Headline: strPtr(r.Headline), Location: strPtr(r.Location),
		Summary: strPtr(r.Summary), Emails: r.Emails, SocialLinks: r.SocialLinks, Articles: r.Articles,
		Mentions: r.Mentions, Companies: r.Companies, Education: r.Education, Skills: r.Skills,
		RawSources: r.RawSources, Status: strPtr(r.Status), CreatedAt: isoformat(r.CreatedAt),
	}, nil
}

// Delete removes the record and reports whether it existed.
func Delete(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	if id > math.MaxInt32 || id < math.MinInt32 {
		return false, nil
	}
	n, err := gen.New(tx).PersonDelete(ctx, int32(id))
	return n > 0, err
}

// JSON renders the detail in Python's key order: stored JSON columns keep their
// stored key order, `x or []` / `x or {}` for null or empty, no HTML escaping.
func (d *Detail) JSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	var err error
	field := func(k string, v []byte) {
		if err != nil {
			return
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + k + `":`)
		b.Write(v)
	}
	scalar := func(k string, v any) {
		var e []byte
		e, err = encodeNoEscape(v)
		if err == nil {
			field(k, e)
		}
	}
	col := func(k string, raw []byte, empty string) {
		if err != nil {
			return
		}
		var n []byte
		n, err = normalizeJSON(raw, empty)
		if err == nil {
			field(k, n)
		}
	}
	scalar("id", d.ID)
	scalar("linkedin_url", d.LinkedInURL)
	scalar("username", d.Username)
	scalar("name", d.Name)
	scalar("headline", d.Headline)
	scalar("location", d.Location)
	scalar("summary", d.Summary)
	col("emails", d.Emails, "[]")
	col("social_links", d.SocialLinks, "{}")
	col("articles", d.Articles, "[]")
	col("mentions", d.Mentions, "[]")
	col("companies", d.Companies, "[]")
	col("education", d.Education, "[]")
	col("skills", d.Skills, "[]")
	col("raw_sources", d.RawSources, "[]")
	scalar("status", d.Status)
	scalar("created_at", d.CreatedAt)
	if err != nil {
		return nil, err
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// normalizeJSON re-encodes stored JSON compactly with key order kept and no
// HTML or ASCII escaping; null and the empty container become empty.
func normalizeJSON(raw []byte, empty string) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte(empty), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b bytes.Buffer
	if err := reencode(dec, &b); err != nil {
		return nil, err
	}
	out := b.Bytes()
	if s := string(out); s == "null" || s == "[]" || s == "{}" || s == `""` || s == "0" || s == "false" {
		return []byte(empty), nil
	}
	return out, nil
}

func reencode(dec *json.Decoder, b *bytes.Buffer) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := t.(type) {
	case json.Delim:
		closer := byte(']')
		if v == '{' {
			closer = '}'
		}
		b.WriteByte(byte(v))
		first := true
		for dec.More() {
			if !first {
				b.WriteByte(',')
			}
			first = false
			if v == '{' {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				kb, _ := encodeNoEscape(kt.(string))
				b.Write(kb)
				b.WriteByte(':')
			}
			if err := reencode(dec, b); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		b.WriteByte(closer)
	case json.Number:
		b.WriteString(strings.TrimSpace(v.String()))
	default:
		eb, err := encodeNoEscape(v)
		if err != nil {
			return err
		}
		b.Write(eb)
	}
	return nil
}

// JSON renders the row without HTML escaping (Python does not escape <, > or
// &); json.Marshal would escape them, so routes use this (or a no-escape
// encoder) rather than json.Marshal.
func (r HistoryRow) JSON() ([]byte, error) { return encodeNoEscape(r) }
