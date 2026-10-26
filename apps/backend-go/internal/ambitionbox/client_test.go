package ambitionbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourceingest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// ---- transcript replay (testdata/vendors/ambitionbox, recorded from Python) ----

type exchange struct {
	Request struct {
		Method       string            `json:"method"`
		URL          string            `json:"url"`
		Headers      map[string]string `json:"headers"`
		JSON         json.RawMessage   `json:"json"`
		TimeoutTotal int               `json:"timeout_total"`
	} `json:"request"`
	Response struct {
		Status  int             `json:"status"`
		Body    json.RawMessage `json:"body"`
		Raises  string          `json:"raises"`
		Message string          `json:"message"`
	} `json:"response"`
}

type transcript struct {
	File     string
	Scenario string `json:"scenario"`
	Call     struct {
		Function string                     `json:"function"`
		Args     map[string]json.RawMessage `json:"args"`
	} `json:"call"`
	Exchanges []exchange      `json:"http_exchanges"`
	Sleeps    []float64       `json:"sleeps"`
	Result    json.RawMessage `json:"result"`
	Raises    *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"raises"`
	next int
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func loadTranscripts(t *testing.T) []*transcript {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "vendors", "ambitionbox", "*.json"))
	if len(files) != 28 {
		t.Fatalf("want 28 transcripts, found %d", len(files))
	}
	var out []*transcript
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var tr transcript
		if err := json.Unmarshal(raw, &tr); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		tr.File = filepath.Base(f)
		out = append(out, &tr)
	}
	return out
}

func (tr *transcript) RoundTrip(req *http.Request) (*http.Response, error) {
	if tr.next >= len(tr.Exchanges) {
		return nil, errors.New("unexpected extra request " + req.Method + " " + req.URL.String())
	}
	ex := tr.Exchanges[tr.next]
	tr.next++
	w := ex.Request
	if req.Method != w.Method || req.URL.String() != w.URL {
		return nil, errors.New(tr.File + ": request " + req.Method + " " + req.URL.String() + " want " + w.Method + " " + w.URL)
	}
	if len(req.Header) != len(w.Headers) {
		return nil, errors.New(tr.File + ": header names differ")
	}
	for k, v := range w.Headers {
		if req.Header.Get(k) != v {
			return nil, errors.New(tr.File + ": header " + k + " differs")
		}
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if len(w.JSON) == 0 || string(w.JSON) == "null" {
		if len(body) != 0 {
			return nil, errors.New(tr.File + ": unexpected body")
		}
	} else {
		o, err := wbcrud.ParseObject(body)
		if err != nil {
			return nil, errors.New(tr.File + ": body not JSON")
		}
		gb, _ := o.MarshalJSON()
		if string(gb) != canonRaw(w.JSON) {
			return nil, errors.New(tr.File + ": body\n got " + string(gb) + "\nwant " + canonRaw(w.JSON))
		}
	}
	if dl, ok := req.Context().Deadline(); !ok || time.Until(dl) > time.Duration(w.TimeoutTotal)*time.Second {
		return nil, errors.New(tr.File + ": missing or too long request deadline")
	}
	if ex.Response.Raises != "" {
		if ex.Response.Raises == "TimeoutError" {
			return nil, timeoutErr{}
		}
		return nil, errors.New(ex.Response.Message)
	}
	text := string(ex.Response.Body)
	if strings.HasPrefix(text, `"`) {
		_ = json.Unmarshal(ex.Response.Body, &text)
	}
	return &http.Response{StatusCode: ex.Response.Status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(text)), Request: req}, nil
}

func canonRaw(raw []byte) string {
	o, err := wbcrud.ParseObject(raw)
	if err != nil {
		return string(raw)
	}
	b, _ := o.MarshalJSON()
	return string(b)
}

type sleepLog struct{ got []time.Duration }

func (s *sleepLog) Sleep(_ context.Context, d time.Duration) error {
	s.got = append(s.got, d)
	return nil
}

func targ[T any](t *testing.T, tr *transcript, name string, def T) T {
	t.Helper()
	raw, ok := tr.Call.Args[name]
	if !ok {
		return def
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: arg %s: %v", tr.File, name, err)
	}
	return v
}

// run performs the transcript's recorded call and returns (ordered result, err).
func (tr *transcript) run(t *testing.T, c *Client) (any, error) {
	ctx := context.Background()
	switch tr.Call.Function {
	case "search_companies":
		return c.SearchCompanies(ctx, SearchParams{
			Page: targ(t, tr, "page", 1), Limit: targ(t, tr, "limit", 0), SortBy: targ(t, tr, "sort_by", ""),
			Industry: targ[[]string](t, tr, "industry", nil), Location: targ[[]string](t, tr, "location", nil),
			CompanyType: targ[[]string](t, tr, "company_type", nil), Rating: targ(t, tr, "rating", ""),
		})
	case "fetch_company_page", "collect_companies":
		return nil, errors.New("handled by collectJSON")
	case "get_company_jobs":
		return c.GetCompanyJobs(ctx, targ(t, tr, "company_id", 0), targ(t, tr, "page", 1))
	case "get_company_detail":
		return c.GetCompanyDetail(ctx, targ(t, tr, "company_id", 0))
	case "get_similar_companies":
		return c.GetSimilarCompanies(ctx, targ(t, tr, "company_id", 0))
	}
	t.Fatalf("%s: unknown function %s", tr.File, tr.Call.Function)
	return nil, nil
}

func pageJSON(t *testing.T, p sourceingest.Page) any {
	t.Helper()
	recs := make([]any, len(p.Records))
	for i, r := range p.Records {
		recs[i] = map[string]any{"provider": r.Provider, "record_id": r.RecordID, "data": r.Data, "rank": r.Rank, "source_url": r.SourceURL}
	}
	return roundTrip(t, map[string]any{"provider": p.Provider, "records": recs, "page": p.Page, "page_size": p.PageSize,
		"source_total": p.SourceTotal, "total_pages": p.TotalPages, "has_more": p.HasMore})
}

func collectionJSON(t *testing.T, c Collection) any {
	t.Helper()
	recs := make([]any, len(c.Records))
	for i, r := range c.Records {
		recs[i] = map[string]any{"provider": r.Provider, "record_id": r.RecordID, "data": r.Data, "rank": r.Rank, "source_url": r.SourceURL}
	}
	return roundTrip(t, map[string]any{"provider": c.Provider, "records": recs, "requested_count": c.RequestedCount,
		"pages_fetched": c.PagesFetched, "next_page": c.NextPage, "source_total": c.SourceTotal, "target_met": c.TargetMet,
		"exhausted": c.Exhausted, "max_pages_reached": c.MaxPagesReached, "warnings": c.Warnings})
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAmbitionBoxTranscripts(t *testing.T) {
	for _, tr := range loadTranscripts(t) {
		tr := tr
		t.Run(tr.File, func(t *testing.T) {
			sl := &sleepLog{}
			c := &Client{HTTP: &http.Client{Transport: tr}, Sleep: sl.Sleep}
			var got any
			var err error
			ctx := context.Background()
			switch tr.Call.Function {
			case "fetch_company_page":
				var p sourceingest.Page
				p, err = c.FetchCompanyPage(ctx, PageParams{Page: targ(t, tr, "page", 1), SortBy: targ(t, tr, "sort_by", ""),
					Industry: targ[[]string](t, tr, "industry", nil), Rating: targ(t, tr, "rating", "")})
				got = pageJSON(t, p)
			case "collect_companies":
				var col Collection
				col, err = c.CollectCompanies(ctx, targ(t, tr, "requested_count", 1), targ(t, tr, "max_pages", 25), nil, nil)
				got = collectionJSON(t, col)
			default:
				got, err = tr.run(t, c)
			}
			if tr.Raises != nil {
				if err == nil {
					t.Fatalf("want %s error", tr.Raises.Type)
				}
				switch tr.Raises.Type {
				case "NotImplementedError":
					var fu *ErrFilterUnsupported
					if !errors.As(err, &fu) || err.Error() != tr.Raises.Message {
						t.Errorf("err %v (%T) want ErrFilterUnsupported %q", err, err, tr.Raises.Message)
					}
				case "RuntimeError":
					var ue *UpstreamError
					if !errors.As(err, &ue) {
						t.Fatalf("err %v (%T) want *UpstreamError", err, err)
					}
					if err.Error() != tr.Raises.Message {
						t.Errorf("message\n got %q\nwant %q", err.Error(), tr.Raises.Message)
					}
				default: // JSONDecodeError: a typed upstream failure, never a panic or a partial result
					var ue *UpstreamError
					if !errors.As(err, &ue) {
						t.Errorf("err %v (%T) want *UpstreamError", err, err)
					}
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				switch tr.Call.Function {
				case "fetch_company_page", "collect_companies":
					var want any
					_ = json.Unmarshal(tr.Result, &want)
					if !reflect.DeepEqual(got, want) {
						gb, _ := json.Marshal(got)
						t.Errorf("result\n got %s\nwant %s", gb, tr.Result)
					}
				default:
					var gs string
					switch g := got.(type) {
					case *wbcrud.Obj:
						b, _ := g.MarshalJSON()
						gs = string(b)
					default:
						gs = string(mustMarshalAny(t, g))
					}
					if want := canonAny(tr.Result); gs != want {
						t.Errorf("result\n got %s\nwant %s", gs, want)
					}
				}
			}
			want := make([]time.Duration, len(tr.Sleeps))
			for i, s := range tr.Sleeps {
				want[i] = time.Duration(s * float64(time.Second))
			}
			if len(want) != len(sl.got) || (len(want) > 0 && !reflect.DeepEqual(want, sl.got)) {
				t.Errorf("sleeps %v want %v", sl.got, want)
			}
			if tr.next != len(tr.Exchanges) {
				t.Errorf("made %d of %d recorded requests", tr.next, len(tr.Exchanges))
			}
		})
	}
}

// mustMarshalAny writes ordered values (Obj inside []any / json.Number) as the API would.
func mustMarshalAny(t *testing.T, v any) []byte {
	t.Helper()
	o := wbcrud.NewObj()
	o.Set("v", v)
	b, err := o.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(bytes.TrimPrefix(b, []byte(`{"v":`)), []byte("}"))
}

func canonAny(raw []byte) string {
	doc, errs := wbcrud.DecodeBody(raw)
	if errs != nil {
		return string(raw)
	}
	o := wbcrud.NewObj()
	o.Set("v", doc)
	b, _ := o.MarshalJSON()
	return strings.TrimSuffix(strings.TrimPrefix(string(b), `{"v":`), "}")
}

// ---- body key order ----

type capture struct {
	body []byte
	url  string
	n    int
}

func (c *capture) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n++
	c.url = req.URL.String()
	if req.Body != nil {
		c.body, _ = io.ReadAll(req.Body)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"cards":[]}`)), Request: req}, nil
}

func TestFetchCompanyPageBodyKeyOrder(t *testing.T) {
	cp := &capture{}
	c := &Client{HTTP: &http.Client{Transport: cp}}
	_, err := c.FetchCompanyPage(context.Background(), PageParams{Page: 2, SortBy: "reviews",
		Industry: []string{"IT Services & Consulting", "HR", "SaaS"}, Rating: "4.5"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"isFilterApplied":true,"page":"2","sortBy":"reviews","limit":20,"industries":["it-services-and-consulting","recruitment","software-product"],"ratings":"4.5"}`
	if string(cp.body) != want {
		t.Errorf("body\n got %s\nwant %s", cp.body, want)
	}
	cp.body = nil
	if _, err := c.FetchCompanyPage(context.Background(), PageParams{}); err != nil {
		t.Fatal(err)
	}
	if want := `{"isFilterApplied":true,"page":"1","sortBy":"popular","limit":20}`; string(cp.body) != want {
		t.Errorf("default body %s want %s", cp.body, want)
	}
	if !strings.HasPrefix(cp.url, "https://www.ambitionbox.com/servicegateway-ambitionbox/") {
		t.Errorf("base not the gateway: %s", cp.url)
	}
}

func TestFilterUnsupportedLocationAndCompanyType(t *testing.T) {
	cp := &capture{}
	c := &Client{HTTP: &http.Client{Transport: cp}}
	for _, tc := range []struct {
		field, msg string
		p          PageParams
	}{
		{"location", "AmbitionBox's gateway ignores location filters -- refusing to return silently unfiltered results", PageParams{Location: []string{"Pune"}}},
		{"company_type", "AmbitionBox's gateway ignores company_type filters -- refusing to return silently unfiltered results", PageParams{CompanyType: []string{"Public"}}},
	} {
		_, err := c.FetchCompanyPage(context.Background(), tc.p)
		var fu *ErrFilterUnsupported
		if !errors.As(err, &fu) || fu.Field != tc.field || err.Error() != tc.msg {
			t.Errorf("%s: err %v", tc.field, err)
		}
	}
	// location wins when both are set, as in Python
	_, err := c.FetchCompanyPage(context.Background(), PageParams{Location: []string{"x"}, CompanyType: []string{"y"}})
	var fu *ErrFilterUnsupported
	if !errors.As(err, &fu) || fu.Field != "location" {
		t.Errorf("both: %v", err)
	}
	if cp.n != 0 {
		t.Errorf("an unsupported filter must not reach the network (%d requests)", cp.n)
	}
}

// ---- recorded fetch vectors ----

func TestFetchCompanyPageVectors(t *testing.T) {
	for _, c := range loadVectors(t).Fetch {
		var args struct {
			Page        int      `json:"page"`
			SortBy      string   `json:"sort_by"`
			Industry    []string `json:"industry"`
			Location    []string `json:"location"`
			CompanyType []string `json:"company_type"`
			Rating      string   `json:"rating"`
		}
		_ = json.Unmarshal(c.Args, &args)
		n := 0
		rt := roundTripFn(func(req *http.Request) (*http.Response, error) {
			n++
			b, _ := io.ReadAll(req.Body)
			w := c.Requests[0]
			if req.Method != w.Method || req.URL.String() != w.URL || canonRaw(b) != canonRaw(w.JSON) {
				t.Errorf("%s: request %s %s %s", c.Name, req.Method, req.URL, b)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(c.ResponseBody)), Request: req}, nil
		})
		cl := &Client{HTTP: &http.Client{Transport: rt}}
		p, err := cl.FetchCompanyPage(context.Background(), PageParams{Page: args.Page, SortBy: args.SortBy, Industry: args.Industry,
			Location: args.Location, CompanyType: args.CompanyType, Rating: args.Rating})
		if c.ErrorType != "" {
			if err == nil || n != len(c.Requests) {
				t.Errorf("%s: err=%v requests=%d", c.Name, err, n)
			}
			if c.ErrorType == "NotImplementedError" && err != nil && err.Error() != c.Error {
				t.Errorf("%s: message %q", c.Name, err)
			}
			continue
		}
		if err != nil || n != len(c.Requests) {
			t.Errorf("%s: err=%v requests=%d", c.Name, err, n)
			continue
		}
		var want any
		_ = json.Unmarshal(c.Value, &want)
		if got := pageJSON(t, p); !reflect.DeepEqual(got, want) {
			gb, _ := json.Marshal(got)
			t.Errorf("%s:\n got %s\nwant %s", c.Name, gb, c.Value)
		}
	}
}

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPageHasMoreVectors(t *testing.T) {
	var v struct {
		H []struct {
			TotalPages *int `json:"total_pages"`
			Page       int  `json:"page"`
			Records    int  `json:"records"`
			PageSize   int  `json:"page_size"`
			HasMore    bool `json:"has_more"`
		} `json:"connector_page_has_more"`
	}
	raw, _ := os.ReadFile("testdata/ambitionbox.json")
	_ = json.Unmarshal(raw, &v)
	if len(v.H) == 0 {
		t.Fatal("no vectors")
	}
	for i, h := range v.H {
		p := sourceingest.NewPage("ambitionbox", make([]sourceingest.Record, h.Records), h.Page, h.PageSize, nil, h.TotalPages)
		if p.HasMore != h.HasMore {
			t.Errorf("vector %d: HasMore=%v want %v", i, p.HasMore, h.HasMore)
		}
	}
}

// ---- collect ----

func scriptedClient(t *testing.T, scripted []json.RawMessage, calls *[]map[string]any) *Client {
	i := 0
	c := &Client{}
	c.fetch = func(_ context.Context, p PageParams) (sourceingest.Page, error) {
		*calls = append(*calls, map[string]any{"page": float64(p.Page), "sort_by": p.SortBy, "industry": strs(p.Industry),
			"location": strs(p.Location), "rating": nilIfEmpty(p.Rating)})
		if i >= len(scripted) {
			t.Fatalf("fetch called %d times, scripted %d", i+1, len(scripted))
		}
		raw := scripted[i]
		i++
		var msg string
		if json.Unmarshal(raw, &msg) == nil {
			return sourceingest.Page{}, errors.New(msg)
		}
		var sp struct {
			IDs         []int `json:"ids"`
			TotalPages  *int  `json:"total_pages"`
			SourceTotal *int  `json:"source_total"`
		}
		_ = json.Unmarshal(raw, &sp)
		recs := make([]sourceingest.Record, len(sp.IDs))
		for k, id := range sp.IDs {
			recs[k] = sourceingest.Record{Provider: "ambitionbox", RecordID: itoa(id), Rank: (p.Page-1)*20 + k, Data: obj1("company_id", id)}
		}
		return sourceingest.NewPage("ambitionbox", recs, p.Page, 20, sp.SourceTotal, sp.TotalPages), nil
	}
	return c
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func strs(s []string) any {
	if s == nil {
		return nil
	}
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func argInt(m map[string]any, k string, def int) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return def
}

func argStrs(m map[string]any, k string) []string {
	var out []string
	if a, ok := m[k].([]any); ok {
		for _, e := range a {
			out = append(out, e.(string))
		}
	}
	return out
}

func runCollect(t *testing.T, c *Client, args map[string]any) (Collection, error) {
	sortBy, _ := args["sort_by"].(string)
	rating, _ := args["rating"].(string)
	return c.CollectCompaniesWith(context.Background(), CollectParams{
		Requested: argInt(args, "requested_count", 0), MaxPages: argInt(args, "max_pages", 25), StartPage: argInt(args, "start_page", 1),
		Industry: argStrs(args, "industry"), Location: argStrs(args, "location"), SortBy: sortBy, Rating: rating})
}

func TestCollectStopsAtTargetOrMaxPages(t *testing.T) {
	for _, c := range loadVectors(t).Collect {
		var calls []map[string]any
		cl := scriptedClient(t, c.Scripted, &calls)
		col, err := runCollect(t, cl, c.Args)
		if c.Error != "" {
			if err == nil || err.Error() != c.Error {
				t.Errorf("%s: err %v want %q", c.Name, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		var ids []any
		var ranks []any
		for _, r := range col.Records {
			ids = append(ids, r.RecordID)
			ranks = append(ranks, float64(r.Rank))
		}
		if ids == nil {
			ids, ranks = []any{}, []any{}
		}
		want := c.Value
		got := map[string]any{"provider": col.Provider, "requested_count": float64(col.RequestedCount), "pages_fetched": float64(col.PagesFetched),
			"next_page": float64(col.NextPage), "source_total": nil, "target_met": col.TargetMet, "exhausted": col.Exhausted,
			"max_pages_reached": col.MaxPagesReached, "warnings": roundTrip(t, append([]string{}, col.Warnings...)),
			"record_ids": ids, "ranks": ranks}
		if col.SourceTotal != nil {
			got["source_total"] = float64(*col.SourceTotal)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", c.Name, got, want)
		}
		if len(calls) != len(c.FetchCalls) {
			t.Errorf("%s: %d fetches want %d", c.Name, len(calls), len(c.FetchCalls))
			continue
		}
		for i := range calls {
			if !reflect.DeepEqual(calls[i], c.FetchCalls[i]) {
				t.Errorf("%s: fetch %d = %v want %v", c.Name, i, calls[i], c.FetchCalls[i])
			}
		}
	}
}

func TestCollectSummaryMatchesVector(t *testing.T) {
	var calls []map[string]any
	scripted := []json.RawMessage{json.RawMessage(`{"ids":[1,2,3],"total_pages":1,"source_total":3}`)}
	cl := scriptedClient(t, scripted, &calls)
	col, err := cl.CollectCompanies(context.Background(), 10, 5, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := col.Summary(false).MarshalJSON()
	want := `{"provider":"ambitionbox","requested_count":10,"returned_count":3,"pages_fetched":1,"next_page":2,"source_total":3,"target_met":false,"exhausted":true,"partial":true,"status":"partial","max_pages_reached":false,"warnings":[]}`
	if string(b) != want {
		t.Errorf("summary\n got %s\nwant %s", b, want)
	}
	// with records the summary appends "records": [data dicts]
	col.Records = []sourceingest.Record{{Data: mustObj(t, []byte(`{"company_id":1,"name":"A"}`))}}
	b, _ = col.Summary(true).MarshalJSON()
	if !strings.HasSuffix(string(b), `"warnings":[],"records":[{"company_id":1,"name":"A"}]}`) {
		t.Errorf("summary with records: %s", b)
	}
	met := Collection{Provider: "ambitionbox", RequestedCount: 1, PagesFetched: 1, NextPage: 2, TargetMet: true, MaxPagesReached: false, Warnings: []string{"w"}}
	b, _ = met.Summary(false).MarshalJSON()
	if !strings.Contains(string(b), `"partial":false,"status":"complete","max_pages_reached":false,"warnings":["w"]`) {
		t.Errorf("complete summary: %s", b)
	}
}

func TestCollectRealRecordsKeepPythonKeyOrder(t *testing.T) {
	body := `{"cards":[{"companyId":7,"name":"Seven","urlName":"seven","companyRating":4.25}],"stats":{"totalCards":1,"totalPages":1}}`
	rt := roundTripFn(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	c := &Client{HTTP: &http.Client{Transport: rt}}
	col, err := c.CollectCompanies(context.Background(), 1, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := col.Summary(true).MarshalJSON()
	if !strings.Contains(string(b), `"records":[{"company_id":7,"name":"Seven","short_name":"","url_name":"seven","logo_url":"","industry":"","rating":4.2,`) {
		t.Errorf("records: %s", b)
	}
	if col.Records[0].RecordID != "7" || col.Records[0].Rank != 0 || col.Records[0].SourceURL == nil {
		t.Errorf("record: %+v", col.Records[0])
	}
}

// ---- retry, cancellation, bounds ----

func TestContextCancelStopsRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	rt := roundTripFn(func(req *http.Request) (*http.Response, error) {
		n++
		return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("down")), Request: req}, nil
	})
	slept := 0
	c := &Client{HTTP: &http.Client{Transport: rt}, Sleep: func(ctx context.Context, _ time.Duration) error {
		slept++
		cancel()
		return ctx.Err()
	}}
	_, err := c.GetCompanyDetail(ctx, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v want context.Canceled", err)
	}
	if n != 1 || slept != 1 {
		t.Errorf("requests=%d sleeps=%d, want 1 and 1", n, slept)
	}
	// a context cancelled before the first request sends nothing
	n = 0
	_, err = c.GetCompanyDetail(ctx, 1)
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Errorf("pre-cancelled: err=%v requests=%d", err, n)
	}
}

func TestContextCancelDuringRequestIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	rt := roundTripFn(func(req *http.Request) (*http.Response, error) {
		n++
		cancel()
		return nil, req.Context().Err()
	})
	c := &Client{HTTP: &http.Client{Transport: rt}, Sleep: func(context.Context, time.Duration) error { t.Fatal("slept"); return nil }}
	if _, err := c.GetCompanyDetail(ctx, 1); !errors.Is(err, context.Canceled) || n != 1 {
		t.Errorf("err=%v n=%d", err, n)
	}
}

func TestResponseBodyBounded(t *testing.T) {
	big := strings.Repeat("a", MaxBody+1)
	n := 0
	rt := roundTripFn(func(req *http.Request) (*http.Response, error) {
		n++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"x":"` + big + `"}`)), Request: req}, nil
	})
	c := &Client{HTTP: &http.Client{Transport: rt}, Sleep: func(context.Context, time.Duration) error { return nil }}
	_, err := c.GetCompanyDetail(context.Background(), 1)
	var ue *UpstreamError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err %v", err)
	}
	if n != 1 {
		t.Errorf("an oversized body must not be retried (%d requests)", n)
	}
	// exactly at the limit is accepted
	pad := strings.Repeat("a", MaxBody-len(`{"x":""}`))
	rt2 := roundTripFn(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"x":"` + pad + `"}`)), Request: req}, nil
	})
	c.HTTP = &http.Client{Transport: rt2}
	if _, err := c.GetCompanyDetail(context.Background(), 1); err != nil {
		t.Errorf("body at the limit: %v", err)
	}
}

func TestRedirectRefusedAndTyped4xx(t *testing.T) {
	hit := 0
	rt := roundTripFn(func(req *http.Request) (*http.Response, error) {
		hit++
		if req.URL.Host != "www.ambitionbox.com" {
			t.Errorf("followed a redirect to %s", req.URL.Host)
		}
		h := http.Header{}
		h.Set("Location", "http://169.254.169.254/latest")
		return &http.Response{StatusCode: 302, Header: h, Body: io.NopCloser(strings.NewReader("moved")), Request: req}, nil
	})
	c := &Client{HTTP: &http.Client{Transport: rt}}
	_, err := c.GetSimilarCompanies(context.Background(), 5)
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Status != 302 || hit != 1 {
		t.Errorf("err=%v hit=%d", err, hit)
	}
}

func obj1(k string, v any) *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set(k, v)
	return o
}

func TestNewEgressPinsGatewayAndGuardsDial(t *testing.T) {
	c := NewEgress("production", nil)
	if c.Base != Gateway || c.HTTP == nil || c.HTTP.Transport == nil {
		t.Fatalf("client %+v", c)
	}
	// a loopback target must be refused by the egress dial guard, not reached
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("loopback reached") }))
	defer srv.Close()
	c.Base = srv.URL
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := c.GetCompanyDetail(context.Background(), 1); err == nil {
		t.Fatal("loopback base must fail")
	}
}

func TestDottedCapitalIMatchesPython(t *testing.T) {
	if got := slugify("İstanbul"); got != "i-stanbul" {
		t.Errorf("slugify=%q", got)
	}
	if got := casefold("İstanbul"); got != "i\u0307stanbul" {
		t.Errorf("casefold=%q", got)
	}
}
