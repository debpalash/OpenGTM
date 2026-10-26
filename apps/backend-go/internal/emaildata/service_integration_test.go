//go:build integration

package emaildata_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emaildata"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// seed ports legacy/python/scripts/contract_seed.py::_seed_connectors (links +
// email_data only) so the recorded /api/data fixtures replay byte for byte.
func seed(t *testing.T) *emaildata.Service {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if _, err := owner.Exec(ctx, `TRUNCATE email_data, links RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	iso := func(tm time.Time) string {
		s := tm.UTC().Format("2006-01-02T15:04:05")
		if us := tm.Nanosecond() / 1000; us != 0 {
			s += fmt.Sprintf(".%06d", us)
		}
		return s + "+00:00"
	}
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	statuses := []string{"Pending", "Processing", "Completed", "Failed"}
	for n := 1; n <= 30; n++ {
		created := iso(base.Add(-time.Duration((n*3)%45)*24*time.Hour - time.Duration(n%24)*time.Hour))
		var src, upd any
		if n%6 != 0 {
			src = "seed"
		}
		if n%2 == 0 {
			upd = created
		}
		if _, err := owner.Exec(ctx, `INSERT INTO links (id, url, status, created_at, source, updated_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			n, fmt.Sprintf("https://source%02d.example.com/list", n), statuses[n%4], created, src, upd); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"José Álvarez", "Zoë Müller", "Łukasz Nowak", "Renée O'Brien", "李雷", "Åsa Lindström", "Chloé Dupont",
		"Ünal Şahin", "Sam \"Sammy\" Lee", "Ana Pérez"}
	domains := []string{"Example.COM", "ACME.io", "Café.Example.fr", "mail.Example.com", "ExAmPlE.org", "acme.io"}
	for i := 1; i <= 60; i++ {
		days := (i*3 + i%5) % 45
		created := base.Add(-time.Duration(days)*24*time.Hour - time.Duration((i*7)%24)*time.Hour - time.Duration((i*11)%60)*time.Minute)
		if i%4 == 0 {
			created = created.Add(time.Duration(i*1111) * time.Microsecond)
		}
		var name, email, createdAt, link, tags, notes any
		switch {
		case i%9 == 0:
		case i == 5:
			name = ""
		default:
			name = names[i%len(names)]
		}
		email = fmt.Sprintf("User%02d@%s", i, domains[i%len(domains)])
		if i == 13 {
			email = "not-an-email"
		}
		if i == 21 {
			email = nil
		}
		if i != 34 {
			createdAt = iso(created)
		}
		switch {
		case i%11 == 0:
		case i == 44:
			link = 999
		default:
			link = (i-1)%30 + 1
		}
		if i%5 != 0 {
			if i%2 == 1 {
				tags = "prospect,vip"
			} else {
				tags = "cold"
			}
		}
		if i%6 == 0 {
			if i%12 == 0 {
				notes = "line one\nline two"
			} else {
				notes = `He said "hi", then left`
			}
		}
		if _, err := owner.Exec(ctx, `INSERT INTO email_data (id, name, email, source_link_id, created_at, is_used, tags, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, i, name, email, link, createdAt, i%3 == 0, tags, notes); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{`SELECT setval(pg_get_serial_sequence('links','id'), 30)`, `SELECT setval(pg_get_serial_sequence('email_data','id'), 60)`} {
		if _, err := owner.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return emaildata.New(pool)
}

func fixture(t *testing.T, name string) contract.Fixture {
	t.Helper()
	return contract.Load(t, "datacrm/"+name)
}

func jsonResp(t *testing.T, v any) contract.Resp {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return contract.Resp{Status: 200, Headers: map[string]string{"content-type": "application/json"}, Body: b}
}

func filterFrom(q url.Values) (emaildata.Filter, int, int) {
	f := emaildata.Filter{SortBy: "id", Order: "desc"}
	if v := q.Get("source_link_id"); v != "" {
		n, _ := strconv.ParseInt(v, 10, 64)
		f.SourceLinkID = &n
	}
	for k, dst := range map[string]**string{"q": &f.Q, "start_date": &f.StartDate, "end_date": &f.EndDate} {
		if q.Has(k) {
			v := q.Get(k)
			*dst = &v
		}
	}
	if v := q.Get("is_used"); v != "" {
		b := v == "true"
		f.IsUsed = &b
	}
	if q.Has("sort_by") {
		f.SortBy = q.Get("sort_by")
	}
	if q.Has("order") {
		f.Order = q.Get("order")
	}
	limit, offset := 50, 0
	if v := q.Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	if v := q.Get("offset"); v != "" {
		offset, _ = strconv.Atoi(v)
	}
	return f, limit, offset
}

// call plays one recorded request against the service and shapes the answer
// the way the route (task 17) will.
func call(t *testing.T, s *emaildata.Service, fx contract.Fixture) contract.Resp {
	t.Helper()
	ctx := context.Background()
	u, err := url.Parse(fx.Request.Path)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	switch {
	case fx.Request.Method == "GET" && u.Path == "/api/data":
		f, l, o := filterFrom(q)
		p, err := s.List(ctx, f, l, o)
		if err != nil {
			t.Fatalf("%s: %v", fx.Request.Path, err)
		}
		return jsonResp(t, p)
	case fx.Request.Method == "GET" && u.Path == "/api/data/export":
		f, _, _ := filterFrom(q)
		var buf bytes.Buffer
		if err := s.Export(ctx, f, &buf); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(buf.String())
		return contract.Resp{Status: 200, Headers: map[string]string{"content-type": "text/csv; charset=utf-8"}, Body: b}
	case u.Path == "/api/data/stats/timeline":
		r, err := s.TimelineStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return jsonResp(t, r)
	case u.Path == "/api/data/stats/domains":
		limit := 10
		if v := q.Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
		}
		r, err := s.DomainStats(ctx, limit)
		if err != nil {
			t.Fatal(err)
		}
		return jsonResp(t, r)
	case u.Path == "/api/data/mark-used":
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(fx.Request.Body, &raw); err != nil {
			t.Fatal(err)
		}
		req := emaildata.MarkUsedRequest{Used: true}
		for k, v := range raw {
			var err error
			switch k {
			case "ids":
				err = json.Unmarshal(v, &req.IDs)
			case "used":
				err = json.Unmarshal(v, &req.Used)
			case "q":
				err = json.Unmarshal(v, &req.Q)
			case "start_date":
				err = json.Unmarshal(v, &req.StartDate)
			case "end_date":
				err = json.Unmarshal(v, &req.EndDate)
			case "source_link_id":
				err = json.Unmarshal(v, &req.SourceLinkID)
			case "is_used_filter":
				err = json.Unmarshal(v, &req.IsUsedFilter)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		n, err := s.MarkUsed(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return jsonResp(t, map[string]any{"count": n, "success": true})
	case fx.Request.Method == "PATCH":
		id, _ := strconv.ParseInt(strings.TrimPrefix(u.Path, "/api/data/"), 10, 64)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(fx.Request.Body, &raw); err != nil {
			t.Fatal(err)
		}
		var p emaildata.Patch
		for k, v := range raw {
			var sv *string
			var bv *bool
			switch k {
			case "name", "email", "tags", "notes":
				if err := json.Unmarshal(v, &sv); err != nil {
					t.Fatal(err)
				}
			case "is_used":
				if err := json.Unmarshal(v, &bv); err != nil {
					t.Fatal(err)
				}
			}
			switch k {
			case "name":
				p.Name = emaildata.OptStr{Set: true, Val: sv}
			case "email":
				p.Email = emaildata.OptStr{Set: true, Val: sv}
			case "tags":
				p.Tags = emaildata.OptStr{Set: true, Val: sv}
			case "notes":
				p.Notes = emaildata.OptStr{Set: true, Val: sv}
			case "is_used":
				p.IsUsed = emaildata.OptBool{Set: true, Val: bv}
			}
		}
		row, err := s.Update(ctx, id, p)
		if err == emaildata.ErrNotFound {
			b, _ := json.Marshal(map[string]string{"detail": emaildata.NotFoundDetail(id)})
			return contract.Resp{Status: 404, Headers: map[string]string{"content-type": "application/json"}, Body: b}
		}
		if err != nil {
			t.Fatal(err)
		}
		return jsonResp(t, map[string]any{"status": "success", "data": row})
	case fx.Request.Method == "DELETE":
		id, _ := strconv.ParseInt(strings.TrimPrefix(u.Path, "/api/data/"), 10, 64)
		found, err := s.Delete(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		st := "not found"
		if found {
			st = "success"
		}
		return jsonResp(t, map[string]string{"status": st})
	}
	t.Fatalf("unhandled fixture %s %s", fx.Request.Method, fx.Request.Path)
	return contract.Resp{}
}

func replay(t *testing.T, s *emaildata.Service, names ...string) {
	t.Helper()
	for _, n := range names {
		fx := fixture(t, n)
		t.Run(n, func(t *testing.T) { contract.AssertEqual(t, call(t, s, fx), fx.Response) })
	}
}

func TestListMatchesFixture(t *testing.T) {
	s := seed(t)
	replay(t, s, "list_default", "list_limit_5_offset_5", "list_offset_beyond_total", "list_sort_id_asc",
		"list_sort_email_asc", "list_sort_email_desc", "list_sort_created_at_asc", "list_sort_created_at_desc",
		"list_order_unknown_is_desc", "list_combined_filters")
}

func TestListItemKeyOrder(t *testing.T) {
	s := seed(t)
	p, err := s.List(context.Background(), emaildata.Filter{SortBy: "id", Order: "asc"}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	want := `{"total":60,"items":[{"id":1,"name":"Zoë Müller","email":"User01@ACME.io","source_link_id":1,"created_at":"2026-08-25T04:49:00+00:00","is_used":false,"tags":"prospect,vip","notes":null,"source_url":"https://source01.example.com/list"}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestListFilters(t *testing.T) {
	s := seed(t)
	replay(t, s, "list_q_name_accent", "list_q_email_case_insensitive", "list_q_no_match", "list_source_link_id",
		"list_source_link_id_zero_ignored", "list_start_date", "list_end_date", "list_date_range", "list_is_used_true", "list_is_used_false")
}

func TestSortByAllowListAndFallback(t *testing.T) {
	s := seed(t)
	replay(t, s, "list_sort_unknown_falls_back_to_id")
	ctx := context.Background()
	for _, col := range []string{"id", "name", "email", "source_link_id", "created_at", "is_used", "tags", "notes"} {
		for _, ord := range []string{"asc", "desc"} {
			if _, err := s.List(ctx, emaildata.Filter{SortBy: col, Order: ord}, 100, 0); err != nil {
				t.Fatalf("%s %s: %v", col, ord, err)
			}
		}
	}
	// descending NULLs first, ascending NULLs last (Postgres default, as Python on Postgres)
	p, _ := s.List(ctx, emaildata.Filter{SortBy: "created_at", Order: "desc"}, 1, 0)
	if p.Items[0].ID != 34 {
		t.Fatalf("desc created_at NULL first: got id %d", p.Items[0].ID)
	}
	p, _ = s.List(ctx, emaildata.Filter{SortBy: "created_at", Order: "asc"}, 100, 0)
	if p.Items[len(p.Items)-1].ID != 34 {
		t.Fatalf("asc created_at NULL last: got id %d", p.Items[len(p.Items)-1].ID)
	}
}

func TestSortByNonColumnAttributeIsNotACrash(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	for _, name := range []string{"metadata", "__table__", "query", "registry", "id; DROP TABLE email_data", "ID", ""} {
		p, err := s.List(ctx, emaildata.Filter{SortBy: name, Order: "asc"}, 3, 0)
		if err != nil {
			t.Fatalf("sort_by=%q: %v", name, err)
		}
		if p.Total != 60 || len(p.Items) != 3 || p.Items[0].ID != 1 {
			t.Fatalf("sort_by=%q: want fallback to id asc, got total=%d first=%v", name, p.Total, p.Items)
		}
	}
	var buf bytes.Buffer
	if err := s.Export(ctx, emaildata.Filter{SortBy: "metadata", Order: "desc"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), emaildata.CSVHeader) {
		t.Fatalf("export with non-column sort_by: %q", buf.String())
	}
}

func TestLimitZeroShape(t *testing.T) {
	s := seed(t)
	replay(t, s, "list_limit_0")
	p, err := s.List(context.Background(), emaildata.Filter{SortBy: "id"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	if string(b) != `{"total":60,"items":[]}` {
		t.Fatalf("got %s", b)
	}
	// an empty page is [] not null
	p, _ = s.List(context.Background(), emaildata.Filter{SortBy: "id", Q: ptr("zzzznomatch")}, 5, 0)
	if b, _ := json.Marshal(p); string(b) != `{"total":0,"items":[]}` {
		t.Fatalf("got %s", b)
	}
}

func TestNegativePagingIsRejected(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	for _, c := range [][2]int{{-1, 0}, {5, -1}} {
		if _, err := s.List(ctx, emaildata.Filter{SortBy: "id"}, c[0], c[1]); err != emaildata.ErrBadPaging {
			t.Fatalf("limit=%d offset=%d: got %v, want ErrBadPaging", c[0], c[1], err)
		}
	}
	errs := emaildata.PagingErrors(-1, -2)
	if len(errs) != 2 || errs[0].Type != "greater_than_equal" || errs[0].Loc[1] != "limit" || errs[1].Loc[1] != "offset" {
		t.Fatalf("PagingErrors: %+v", errs)
	}
	if emaildata.PagingErrors(0, 0) != nil {
		t.Fatal("0/0 must be valid")
	}
}

func TestMarkUsedByIDs(t *testing.T) {
	s := seed(t)
	replay(t, s, "mark_used_ids", "list_after_mark_ids", "mark_used_ids_unused")
	// ids beyond the int4 range can never match (Python raises a driver error)
	n, err := s.MarkUsed(context.Background(), emaildata.MarkUsedRequest{IDs: []int64{1, 1 << 40}, Used: true})
	if err != nil || n != 1 {
		t.Fatalf("out-of-range id: n=%d err=%v", n, err)
	}
}

func TestMarkUsedBulkFilterAndEverything(t *testing.T) {
	s := seed(t)
	replay(t, s, "mark_used_ids", "mark_used_ids_unused", "mark_used_filter_q", "mark_used_filter_source_link",
		"mark_used_filter_dates_and_used", "mark_used_filter_no_match", "list_after_mark_filters",
		"mark_used_empty_marks_every_row", "mark_used_empty_unmarks_every_row")
}

func TestUpdateExcludeUnset(t *testing.T) {
	s := seed(t)
	replay(t, s, "mark_used_ids", "mark_used_ids_unused", "mark_used_filter_q", "mark_used_filter_source_link",
		"mark_used_filter_dates_and_used", "mark_used_empty_marks_every_row", "mark_used_empty_unmarks_every_row",
		"patch_ok", "patch_partial_email_only", "patch_null_fields", "patch_empty_body", "patch_unknown_field_ignored")
	row, err := s.Update(context.Background(), 1, emaildata.Patch{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(row)
	if !strings.HasPrefix(string(b), `{"id":1,"name":"Zoë Ünal","email":`) {
		t.Fatalf("row key order / values: %s", b)
	}
	// explicit null on is_used sets NULL
	row, err = s.Update(context.Background(), 1, emaildata.Patch{IsUsed: emaildata.OptBool{Set: true}})
	if err != nil || row.IsUsed != nil {
		t.Fatalf("is_used null: %+v %v", row, err)
	}
}

func TestUpdateMissingIs404Detail(t *testing.T) {
	s := seed(t)
	replay(t, s, "patch_not_found")
	if _, err := s.Update(context.Background(), 1<<40, emaildata.Patch{}); err != emaildata.ErrNotFound {
		t.Fatalf("out-of-range id: %v", err)
	}
	if got := emaildata.NotFoundDetail(9999); got != "Record with id 9999 not found" {
		t.Fatal(got)
	}
}

func TestDeleteMissingStatus(t *testing.T) {
	s := seed(t)
	replay(t, s, "delete_ok", "delete_again_not_found", "list_total_after_delete")
	if found, err := s.Delete(context.Background(), -5); err != nil || found {
		t.Fatalf("negative id: %v %v", found, err)
	}
	if found, err := s.Delete(context.Background(), 1<<40); err != nil || found {
		t.Fatalf("huge id: %v %v", found, err)
	}
}

func TestExportBytesMatchFixture(t *testing.T) {
	s := seed(t)
	replay(t, s, "export_all", "export_filtered", "export_empty")
	if got := emaildata.ExportFilename(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)); got != "export_20260901_120000.csv" {
		t.Fatal(got)
	}
}

type vecRow struct {
	Name         *string `json:"name"`
	Email        *string `json:"email"`
	Tags         *string `json:"tags"`
	Notes        *string `json:"notes"`
	SourceLinkID *int64  `json:"source_link_id"`
	CreatedAt    any     `json:"created_at"`
	IsUsed       any     `json:"is_used"`
}

func (v vecRow) row() emaildata.Row {
	r := emaildata.Row{Name: v.Name, Email: v.Email, Tags: v.Tags, Notes: v.Notes, SourceLinkID: v.SourceLinkID}
	if v.CreatedAt != nil {
		s := fmt.Sprint(v.CreatedAt)
		if f, ok := v.CreatedAt.(float64); ok {
			s = strconv.FormatInt(int64(f), 10)
		}
		r.CreatedAt = &s
	}
	switch u := v.IsUsed.(type) {
	case bool:
		r.IsUsed = &u
	case float64:
		b := u != 0
		r.IsUsed = &b
	}
	return r
}

type csvVectors struct {
	Header string `json:"header"`
	Cases  []struct {
		Name          string   `json:"name"`
		Rows          []vecRow `json:"rows"`
		GeneratedRows *struct {
			Count int `json:"count"`
		} `json:"generated_rows"`
		Result struct {
			Body      string `json:"body"`
			LineCount int    `json:"line_count"`
			Sha256    string `json:"sha256"`
		} `json:"result"`
	} `json:"cases"`
}

func TestExportEscaping(t *testing.T) {
	raw, err := os.ReadFile("testdata/emaildata_csv.json")
	if err != nil {
		t.Fatal(err)
	}
	var v csvVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Header != emaildata.CSVHeader {
		t.Fatalf("header %q", emaildata.CSVHeader)
	}
	ran := 0
	for _, c := range v.Cases {
		var rows []emaildata.Row
		switch {
		case c.GeneratedRows != nil:
			for i := 0; i < c.GeneratedRows.Count; i++ {
				n, e, tm := fmt.Sprintf("N%d", i), fmt.Sprintf("e%d@x.io", i), "2026-08-01"
				u := i%2 == 0
				r := emaildata.Row{Name: &n, Email: &e, CreatedAt: &tm, IsUsed: &u}
				if i != 0 {
					l := int64(i)
					r.SourceLinkID = &l
				}
				rows = append(rows, r)
			}
		default:
			for _, vr := range c.Rows {
				rows = append(rows, vr.row())
			}
		}
		var buf bytes.Buffer
		if err := emaildata.WriteCSV(&buf, rows); err != nil {
			t.Fatal(err)
		}
		if c.GeneratedRows != nil {
			sum := sha256.Sum256(buf.Bytes())
			if hex.EncodeToString(sum[:]) != c.Result.Sha256 || strings.Count(buf.String(), "\n") != c.Result.LineCount {
				t.Errorf("%s: sha/lines mismatch", c.Name)
			}
		} else if buf.String() != c.Result.Body {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, buf.String(), c.Result.Body)
		}
		ran++
	}
	if ran < 18 {
		t.Fatalf("only %d vector cases ran", ran)
	}
}

func TestExportBatches(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	owner, _ := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	defer owner.Close()
	if _, err := owner.Exec(ctx, `INSERT INTO email_data (name, email, created_at, is_used)
		SELECT 'n'||g, 'e'||g||'@x.io', '2026-01-01', false FROM generate_series(1, 2500) g`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Export(ctx, emaildata.Filter{SortBy: "id", Order: "asc"}, &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	// 60 seeded rows contain two multi-line notes? notes newlines become spaces, so one line per row
	if len(lines) != 1+60+2500 {
		t.Fatalf("lines %d", len(lines))
	}
	if !strings.HasPrefix(lines[len(lines)-1], `"n2500","e2500@x.io"`) {
		t.Fatalf("last line %q", lines[len(lines)-1])
	}
}

func TestTimelineStatsMatchFixture(t *testing.T) {
	s := seed(t)
	replay(t, s, "stats_timeline", "stats_domains_default", "stats_domains_limit_2", "stats_domains_limit_0", "stats_domains_limit_negative")
}

func TestDomainStatsOrderingTies(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	owner, _ := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	defer owner.Close()
	if _, err := owner.Exec(ctx, `TRUNCATE email_data RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"a@Zeta.com", "b@alpha.com", "c@ALPHA.com", "d@zeta.com", "e@mid.org", "no-at", "x@y@last.net", "", "q@mid.org", "z@beta.io"} {
		if _, err := owner.Exec(ctx, `INSERT INTO email_data (email) VALUES (NULLIF($1,''))`, e); err != nil {
			t.Fatal(err)
		}
	}
	owner.Exec(ctx, `INSERT INTO email_data (email) VALUES (NULL)`)
	got, err := s.DomainStats(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	// count desc, ties by first appearance: zeta (2, first), alpha (2), mid (2), last.net, beta.io
	want := `[{"domain":"zeta.com","count":2},{"domain":"alpha.com","count":2},{"domain":"mid.org","count":2},{"domain":"last.net","count":1},{"domain":"beta.io","count":1}]`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	got, _ = s.DomainStats(ctx, 2)
	if len(got) != 2 {
		t.Fatalf("limit 2: %v", got)
	}
	for _, n := range []int{0, -3} {
		got, err = s.DomainStats(ctx, n)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("limit %d: %v %v", n, got, err)
		}
	}
	// timeline sorts by string, empty/NULL created_at dropped
	if _, err := owner.Exec(ctx, `UPDATE email_data SET created_at = CASE id WHEN 1 THEN '2026-02-01T00' WHEN 2 THEN '2026-01-09' WHEN 3 THEN '2026-02-01x' WHEN 4 THEN '' ELSE NULL END`); err != nil {
		t.Fatal(err)
	}
	tl, err := s.TimelineStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tb, _ := json.Marshal(tl)
	if string(tb) != `[{"date":"2026-01-09","count":1},{"date":"2026-02-01","count":2}]` {
		t.Fatalf("timeline %s", tb)
	}
	_ = sort.Strings
}

func TestLikeWildcardsAreParity(t *testing.T) {
	s := seed(t)
	replay(t, s, "list_q_percent_wildcard")
	ctx := context.Background()
	for q, want := range map[string]int64{"%": 60, "_": 60, `\%`: 0, "ACME.IO": 20, "": 60} {
		p, err := s.List(ctx, emaildata.Filter{SortBy: "id", Q: ptr(q)}, 1, 0)
		if err != nil {
			t.Fatalf("q=%q: %v", q, err)
		}
		if p.Total != want {
			t.Errorf("q=%q total %d want %d", q, p.Total, want)
		}
	}
	if _, err := s.List(ctx, emaildata.Filter{SortBy: "id", Q: ptr("a\x00b")}, 1, 0); err != emaildata.ErrInvalidInput {
		t.Fatalf("NUL byte: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
