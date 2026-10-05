package audiencerefresh

import (
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
)

func filterOf(t *testing.T, raw string) (*leadFilter, error) {
	t.Helper()
	v, err := jobkit.Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return buildLeadFilter(v, 2)
}

func TestBuildLeadFilterMatchesTheLeadStore(t *testing.T) {
	// Falsy filters (and an absent/null document) apply no condition.
	for _, raw := range []string{`null`, `{}`, `{"city": "", "status": null, "job_ids": [], "has_email": null,
		"min_score": null, "search": "", "specialization": 0, "lead_ids": null, "unknown": 5}`, `[]`, `0`} {
		f, err := filterOf(t, raw)
		if err != nil || len(f.conds) != 0 {
			t.Errorf("%s: conds=%v err=%v, want none", raw, f.conds, err)
		}
	}

	f, err := filterOf(t, `{"lead_ids": [3, 1], "city": "Pune", "state": "MH", "score_tier": "hot", "status": "new",
		"source": "csv", "company_size": "1-50", "job_ids": ["J1"], "specialization": "staff",
		"has_email": true, "has_phone": false, "has_website": true, "min_score": 70, "max_score": 90.5, "search": "acme"}`)
	if err != nil {
		t.Fatal(err)
	}
	sql := f.where()
	for _, want := range []string{
		"l.id = ANY($3::bigint[])", "l.city = $4", "l.state = $5", "l.score_tier = $6", "l.status = $7", "l.source = $8",
		"l.company_size = $9", "l.collection_job_id = ANY($10::text[]) OR l.source = ANY($11::text[])",
		"l.specialization ILIKE $12", "(l.email IS NOT NULL AND l.email <> '')", "(l.phone IS NULL OR l.phone = '')",
		"(l.website IS NOT NULL AND l.website <> '')", "l.score >= $13::text::numeric", "l.score <= $14::text::numeric",
		"l.search_tsv @@ websearch_to_tsquery('english', $15)", "l.company ILIKE $16", "l.description ILIKE $16",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in %s", want, sql)
		}
	}
	wantArgs := []any{nil, nil, []int64{3, 1}, "Pune", "MH", "hot", "new", "csv", "1-50", []string{"J1"}, []string{"job:J1"},
		"%staff%", "70", "90.5", "acme", "%acme%"}
	if !reflect.DeepEqual(f.args, wantArgs) {
		t.Errorf("args = %#v\nwant   %#v", f.args, wantArgs)
	}

	// A has_* flag only counts when it is exactly true or false.
	if f, err := filterOf(t, `{"has_email": 1, "has_phone": "yes", "has_website": []}`); err != nil || len(f.conds) != 0 {
		t.Errorf("non-boolean flags must be ignored: %v %v", f.conds, err)
	}
	// min_score: 0 is a bound (`is not None`), unlike the truthy filters.
	if f, err := filterOf(t, `{"min_score": 0}`); err != nil || len(f.conds) != 1 {
		t.Errorf("min_score 0 must apply: %v %v", f.conds, err)
	}
	// list(job_ids) semantics: a string iterates its characters.
	if f, err := filterOf(t, `{"job_ids": "ab"}`); err != nil || !reflect.DeepEqual(f.args[2], []string{"a", "b"}) {
		t.Errorf("job_ids string: %v %v", f.args, err)
	}
	// An empty lead_ids list matches nothing (it is not "no filter").
	if f, err := filterOf(t, `{"lead_ids": []}`); err != nil || len(f.conds) != 1 {
		t.Errorf("empty lead_ids must still filter: %v %v", f.conds, err)
	}
}

func TestBuildLeadFilterRejectsWhatTheDatabaseWouldReject(t *testing.T) {
	for raw, want := range map[string]string{
		`{"lead_ids": "abc"}`:                  "ArgumentError",
		`{"lead_ids": [1, "x"]}`:               "ProgrammingError",
		`{"lead_ids": [1.5]}`:                  "ProgrammingError",
		`{"status": 5}`:                        "ProgrammingError",
		`{"city": ["a"]}`:                      "ProgrammingError",
		`{"job_ids": 5}`:                       "TypeError",
		`{"job_ids": [5]}`:                     "ProgrammingError",
		`{"min_score": true}`:                  "ProgrammingError",
		`{"max_score": []}`:                    "ProgrammingError",
		`{"specialization": [1]}`:              "TypeError",
		`{"search": {"a": 1}}`:                 "TypeError",
		`[1]`:                                  "AttributeError",
		`"text"`:                               "AttributeError",
		`5`:                                    "AttributeError",
		`{"lead_ids": [99999999999999999999]}`: "DataError",
	} {
		_, err := filterOf(t, raw)
		if err == nil || !strings.HasPrefix(jobkit.Describe(err), want+":") {
			t.Errorf("%s: %v, want a %s", raw, err, want)
		}
	}
	if _, err := filterOf(t, `[1]`); err == nil || err.Error() != "'list' object has no attribute 'get'" {
		t.Errorf("filters that are not an object: %v", err)
	}
}

func TestSnapshotCoversExactlyTheLeadDataclass(t *testing.T) {
	// 48 fields in the Lead dataclass; json_build_object takes at most 100 arguments.
	if len(leadFields) != 48 || len(leadFields)*2 > 100 {
		t.Fatalf("%d lead fields", len(leadFields))
	}
	seen := map[string]bool{}
	for _, f := range leadFields {
		if seen[f] {
			t.Errorf("duplicate field %s", f)
		}
		seen[f] = true
	}
	for _, notInDataclass := range []string{"founding_year", "last_funding_amount", "investors", "recent_news", "google_rating",
		"revenue_estimate", "email_verify", "email_presence", "search_tsv"} {
		if seen[notInDataclass] {
			t.Errorf("%s is a leads column the Lead dataclass drops; it must not reach a snapshot", notInDataclass)
		}
	}
	sql := snapshotSQL("$2")
	if !strings.Contains(sql, "COALESCE(NULLIF(l.created_at, ''), $2::text)") || !strings.Contains(sql, "COALESCE(NULLIF(l.updated_at, ''), $2::text)") {
		t.Error("created_at/updated_at must fall back to the clock when falsy")
	}
}
