//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

type jobRow struct {
	typ, executor, status, payload string
	ws                             *string
	fireKeyNull                    bool
	priority, maxRetries, retries  int
}

func (e leadsEnv) jobs(t *testing.T) []jobRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT type, executor, status, payload::text, workspace_id,
	  fire_key IS NULL, priority, max_retries, retry_count FROM jobs WHERE type='bulk_enrich' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.typ, &j.executor, &j.status, &j.payload, &j.ws, &j.fireKeyNull, &j.priority, &j.maxRetries, &j.retries); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func (e leadsEnv) clearBulkJobs(t *testing.T) {
	t.Helper()
	del := func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM jobs WHERE type='bulk_enrich'`) }
	del()
	t.Cleanup(del)
}

// payloadKeys returns the top-level keys of a JSON object in document order.
func payloadKeys(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("payload not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestBulkEnrichEnqueuesGoJob(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", `{"lead_ids":[3,1,2],"action":"scrape_website"}`)
	m := regexp.MustCompile(`^\{"ok":true,"job_id":"([0-9a-f]{32})","count":3,"action":"scrape_website"\}$`).FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	js := e.jobs(t)
	if len(js) != 1 {
		t.Fatalf("jobs = %d", len(js))
	}
	j := js[0]
	// Python (add_job) shape with executor go (the only one since the plan 15 cutover): priority 1, pending, max_retries 3, NULL fire_key, workspace_id from payload.
	if j.executor != "go" || j.status != "pending" || j.priority != 1 || j.maxRetries != 3 || j.retries != 0 || !j.fireKeyNull ||
		j.ws == nil || *j.ws != "ws-alpha" {
		t.Fatalf("row %+v", j)
	}
	if got := strings.Join(payloadKeys(t, j.payload), ","); got != "job_id,ids,action,workspace_id,slug" {
		t.Fatalf("payload key order %s", got)
	}
	var p struct {
		JobID string `json:"job_id"`
		IDs   []int  `json:"ids"`
		Act   string `json:"action"`
		WS    string `json:"workspace_id"`
		Slug  string `json:"slug"`
	}
	if err := json.Unmarshal([]byte(j.payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.JobID != m[1] || len(p.IDs) != 3 || p.IDs[0] != 3 || p.IDs[1] != 1 || p.IDs[2] != 2 || p.Act != "scrape_website" ||
		p.WS != "ws-alpha" || p.Slug == "" {
		t.Fatalf("payload %s", j.payload)
	}
}

func TestBulkEnrichDefaultActionAndCoercion(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	// pydantic lax ints: "3", 4.0, true, " 7 ", "1_000", "5.0"; duplicates collapse after coercion (1 == true == "1").
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", `{"lead_ids":[1,"1",true,"3",4.0," 7 ","1_000","5.0",-3,"+4",1e2]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":8,"action":"find_emails"}`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var p struct {
		IDs []int `json:"ids"`
	}
	_ = json.Unmarshal([]byte(e.jobs(t)[0].payload), &p)
	if got := fmt.Sprint(p.IDs); got != "[1 3 4 7 1000 5 -3 100]" {
		t.Fatalf("ids %s", got)
	}
}

func TestBulkEnrichDedupAndCap100(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	var ids []string
	for i := 1; i <= 130; i++ {
		ids = append(ids, "5", jsonInt(i)) // every id preceded by a duplicate 5
	}
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", `{"lead_ids":[`+strings.Join(ids, ",")+`]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":100,`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var p struct {
		IDs []int `json:"ids"`
	}
	_ = json.Unmarshal([]byte(e.jobs(t)[0].payload), &p)
	if len(p.IDs) != 100 || p.IDs[0] != 5 || p.IDs[1] != 1 || p.IDs[4] != 4 || p.IDs[99] != 100 {
		t.Fatalf("ids %v", p.IDs)
	}
}

func jsonInt(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestBulkEnrichEmpty400(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	for _, body := range []string{`{"lead_ids":[]}`, `{"lead_ids":[],"action":"bogus"}`} {
		rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", body)
		if rec.Code != 400 || rec.Body.String() != `{"detail":"lead_ids required"}` {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if n := len(e.jobs(t)); n != 0 {
		t.Errorf("%d jobs enqueued", n)
	}
}

func TestBulkEnrichBadAction400(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", `{"lead_ids":[1],"action":"Find_Emails"}`)
	if rec.Code != 400 || rec.Body.String() != `{"detail":"action must be 'find_emails' or 'scrape_website'"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := len(e.jobs(t)); n != 0 {
		t.Errorf("%d jobs enqueued", n)
	}
}

func TestBulkEnrichBodyNotInts422(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	// Expected bodies are from pydantic 2 (legacy/python, FastAPI adds the "body" loc prefix).
	cases := []struct{ body, want string }{
		{`{"lead_ids":[1.5]}`, `{"detail":[{"type":"int_from_float","loc":["body","lead_ids",0],"msg":"Input should be a valid integer, got a number with a fractional part","input":1.5}]}`},
		{`{"lead_ids":["x"],"action":1}`, `{"detail":[{"type":"int_parsing","loc":["body","lead_ids",0],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"x"},{"type":"string_type","loc":["body","action"],"msg":"Input should be a valid string","input":1}]}`},
		{`{"lead_ids":[null]}`, `{"detail":[{"type":"int_type","loc":["body","lead_ids",0],"msg":"Input should be a valid integer","input":null}]}`},
		{`{"lead_ids":"abc"}`, `{"detail":[{"type":"list_type","loc":["body","lead_ids"],"msg":"Input should be a valid list","input":"abc"}]}`},
		{`{"lead_ids":["1e2"]}`, `{"detail":[{"type":"int_parsing","loc":["body","lead_ids",0],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"1e2"}]}`},
		{`{"lead_ids":[[1]]}`, `{"detail":[{"type":"int_type","loc":["body","lead_ids",0],"msg":"Input should be a valid integer","input":[1]}]}`},
		{`{}`, `{"detail":[{"type":"missing","loc":["body","lead_ids"],"msg":"Field required","input":{}}]}`},
		{`{"lead_ids":[1],"action":null}`, `{"detail":[{"type":"string_type","loc":["body","action"],"msg":"Input should be a valid string","input":null}]}`},
	}
	for _, c := range cases {
		rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", c.body)
		if rec.Code != 422 || rec.Body.String() != c.want {
			t.Errorf("%s:\n got %d %s\nwant %s", c.body, rec.Code, rec.Body.String(), c.want)
		}
	}
	if n := len(e.jobs(t)); n != 0 {
		t.Errorf("%d jobs enqueued", n)
	}
}

// Lead ids outside int4 are rejected with 422 (Python enqueues them verbatim and the worker 500s on
// the Postgres lookup); nothing is enqueued and huge digit strings are never parsed.
func TestBulkEnrichIDOutsideInt4Is422(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	const want = `{"detail":[{"type":"value_error","loc":["body","lead_ids",%d],"msg":"Value error, lead id out of range","input":null}]}`
	huge := strings.Repeat("9", 200000)
	cases := []struct {
		body string
		idx  int
	}{
		{`{"lead_ids":[2147483648]}`, 0},
		{`{"lead_ids":[1,-2147483649]}`, 1},
		{`{"lead_ids":[123456789012345678901234567890]}`, 0},
		{`{"lead_ids":["2147483648"]}`, 0},
		{`{"lead_ids":[1e10]}`, 0},
		{`{"lead_ids":[` + huge + `]}`, 0},
		{`{"lead_ids":["` + huge + `"]}`, 0},
	}
	for _, c := range cases {
		rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", c.body)
		if w := fmt.Sprintf(want, c.idx); rec.Code != 422 || rec.Body.String() != w {
			t.Errorf("%.40s: got %d %.200s", c.body, rec.Code, rec.Body.String())
		}
	}
	for _, ok := range []string{`{"lead_ids":[2147483647]}`, `{"lead_ids":[-2147483648]}`} {
		if rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", ok); rec.Code != 200 {
			t.Errorf("%s = %d %s", ok, rec.Code, rec.Body.String())
		}
	}
	if n := len(e.jobs(t)); n != 2 {
		t.Errorf("%d jobs enqueued, want 2", n)
	}
}

func TestBulkEnrichRequiresEditor(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	rec := e.send("bob", "ws-alpha", "POST", "/api/leads/bulk-enrich", `{"lead_ids":[1]}`)
	if rec.Code != 403 || rec.Body.String() != `{"detail":"Workspace permission denied: tables.write"}` {
		t.Errorf("viewer: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("", "", "POST", "/api/leads/bulk-enrich", `{"lead_ids":[1]}`); rec.Code != 401 {
		t.Errorf("anon: %d", rec.Code)
	}
	if n := len(e.jobs(t)); n != 0 {
		t.Errorf("%d jobs enqueued", n)
	}
}

func TestBulkEnrichEnqueueFailureIs500NoLeak(t *testing.T) {
	e := newLeadsEnv(t)
	e.clearBulkJobs(t)
	old := enqueueJob
	enqueueJob = func(context.Context, gen.DBTX, string, string, any, string) (int64, error) {
		return 0, errors.New("dial tcp 10.9.8.7:5432: secret-host-detail")
	}
	t.Cleanup(func() { enqueueJob = old })
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/bulk-enrich", `{"lead_ids":[1]}`)
	if rec.Code != 500 || rec.Body.String() != `{"detail":"Internal Server Error"}` || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
