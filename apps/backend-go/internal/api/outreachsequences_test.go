package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func seqErrs(t *testing.T, errs []httpx.FieldError) string {
	t.Helper()
	b, err := json.Marshal(errs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCreateDefaults(t *testing.T) {
	c, errs := parseSeqCreate([]byte(`{"name":"n"}`))
	if errs != nil {
		t.Fatal(seqErrs(t, errs))
	}
	in := c.In
	if in.Name != "n" || in.Description != "" || in.DailyLimit != 50 || in.SendWindowStart != 9 || in.SendWindowEnd != 18 ||
		in.SendWindowTZ != "UTC" || in.ConsentBasis != "" || string(in.Steps) != "[]" {
		t.Fatalf("defaults: %+v steps=%s", in, in.Steps)
	}
}

func TestCreateStepsKeepPythonKeyOrderAndDefaultDelay(t *testing.T) {
	c, errs := parseSeqCreate([]byte(`{"name":"n","steps":[{"delay_hours":"3","subject":"S <x> & y","body_html":"<p>b</p>","step_number":1.0,"extra":1}]}`))
	if errs != nil {
		t.Fatal(seqErrs(t, errs))
	}
	want := `[{"step_number":1,"subject":"S <x> & y","body_html":"<p>b</p>","delay_hours":3}]`
	if string(c.In.Steps) != want {
		t.Fatalf("steps = %s want %s", c.In.Steps, want)
	}
}

func TestCreateMissingAndTypeErrors(t *testing.T) {
	_, errs := parseSeqCreate([]byte(`{"description":"x"}`))
	if got := seqErrs(t, errs); !strings.Contains(got, `"type":"missing","loc":["body","name"]`) {
		t.Fatalf("missing name: %s", got)
	}
	_, errs = parseSeqCreate([]byte(`{"name":"x","daily_limit":"many"}`))
	want := `[{"type":"int_parsing","loc":["body","daily_limit"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"many"}]`
	if got := seqErrs(t, errs); got != want {
		t.Fatalf("got %s", got)
	}
	_, errs = parseSeqCreate([]byte(`{"name":"x","steps":[{"step_number":0}]}`))
	got := seqErrs(t, errs)
	if !strings.Contains(got, `"loc":["body","steps",0,"subject"]`) || !strings.Contains(got, `"loc":["body","steps",0,"body_html"]`) {
		t.Fatalf("step errors: %s", got)
	}
	_, errs = parseSeqCreate([]byte(`{"name":5}`))
	if got := seqErrs(t, errs); !strings.Contains(got, `"string_type"`) {
		t.Fatalf("name type: %s", got)
	}
}

func TestOverlongFieldsAre422NotPostgresErrors(t *testing.T) {
	cases := map[string]string{
		"name empty":       `{"name":""}`,
		"name long":        `{"name":"` + strings.Repeat("n", 201) + `"}`,
		"description long": `{"name":"n","description":"` + strings.Repeat("d", 5001) + `"}`,
		"tz long":          `{"name":"n","send_window_tz":"` + strings.Repeat("z", 41) + `"}`,
		"consent long":     `{"name":"n","consent_basis":"` + strings.Repeat("c", 41) + `"}`,
		"start high":       `{"name":"n","send_window_start":24}`,
		"end negative":     `{"name":"n","send_window_end":-1}`,
		"limit zero":       `{"name":"n","daily_limit":0}`,
		"limit huge":       `{"name":"n","daily_limit":100001}`,
		"limit overflow":   `{"name":"n","daily_limit":99999999999}`,
		"delay high":       `{"name":"n","steps":[{"step_number":0,"subject":"s","body_html":"b","delay_hours":8761}]}`,
		"delay negative":   `{"name":"n","steps":[{"step_number":0,"subject":"s","body_html":"b","delay_hours":-1}]}`,
		"too many steps":   `{"name":"n","steps":[` + strings.TrimSuffix(strings.Repeat(`{"step_number":0,"subject":"s","body_html":"b"},`, 51), ",") + `]}`,
		"name NUL":         `{"name":"a\u0000b"}`,
		"description NUL":  `{"name":"n","description":"a\u0000b"}`,
	}
	for name, body := range cases {
		_, errs := parseSeqCreate([]byte(body))
		if len(errs) == 0 {
			t.Errorf("%s: accepted", name)
			continue
		}
		for _, e := range errs {
			if e.Type == "" || e.Msg == "" || len(e.Loc) < 2 {
				t.Errorf("%s: malformed error %+v", name, e)
			}
		}
	}
	// boundary values are accepted
	ok := `{"name":"` + strings.Repeat("n", 200) + `","description":"` + strings.Repeat("d", 5000) + `","send_window_tz":"America/Argentina/ComodRivadavia",` +
		`"consent_basis":"` + strings.Repeat("c", 40) + `","send_window_start":0,"send_window_end":23,"daily_limit":100000,` +
		`"steps":[{"step_number":0,"subject":"s","body_html":"b","delay_hours":8760}]}`
	if _, errs := parseSeqCreate([]byte(ok)); errs != nil {
		t.Fatalf("boundaries rejected: %s", seqErrs(t, errs))
	}
	// fifty steps is the cap
	fifty := `{"name":"n","steps":[` + strings.TrimSuffix(strings.Repeat(`{"step_number":0,"subject":"s","body_html":"b"},`, 50), ",") + `]}`
	if _, errs := parseSeqCreate([]byte(fifty)); errs != nil {
		t.Fatalf("50 steps rejected: %s", seqErrs(t, errs))
	}
	// the error carries pydantic's ctx and message
	_, errs := parseSeqCreate([]byte(`{"name":"n","daily_limit":0}`))
	want := `[{"type":"greater_than_equal","loc":["body","daily_limit"],"msg":"Input should be greater than or equal to 1","input":0,"ctx":{"ge":1}}]`
	if got := seqErrs(t, errs); got != want {
		t.Fatalf("got %s", got)
	}
	_, errs = parseSeqCreate([]byte(`{"name":""}`))
	if got := seqErrs(t, errs); !strings.Contains(got, `"string_too_short"`) || !strings.Contains(got, `"min_length":1`) {
		t.Fatalf("got %s", got)
	}
	_, errs = parseSeqUpdate([]byte(`{"name":"` + strings.Repeat("n", 201) + `","daily_limit":100001,"send_window_start":99}`))
	if len(errs) != 3 {
		t.Fatalf("update errors: %s", seqErrs(t, errs))
	}
}

func TestInvalidTimezone422(t *testing.T) {
	for _, tz := range []string{"Mars/Olympus", "", "Local", "../etc/passwd", "utc/../x", "Europe/", "/UTC"} {
		b, _ := json.Marshal(map[string]any{"name": "n", "send_window_tz": tz})
		if _, errs := parseSeqCreate(b); len(errs) == 0 {
			t.Errorf("create accepted tz %q", tz)
		}
		b, _ = json.Marshal(map[string]any{"send_window_tz": tz})
		if _, errs := parseSeqUpdate(b); len(errs) == 0 {
			t.Errorf("update accepted tz %q", tz)
		}
	}
	for _, tz := range []string{"UTC", "Asia/Kolkata", "America/New_York", "Europe/Berlin"} {
		b, _ := json.Marshal(map[string]any{"name": "n", "send_window_tz": tz})
		if _, errs := parseSeqCreate(b); errs != nil {
			t.Errorf("tz %q rejected: %s", tz, seqErrs(t, errs))
		}
	}
	_, errs := parseSeqCreate([]byte(`{"name":"n","send_window_tz":"Mars/Olympus"}`))
	if got := seqErrs(t, errs); !strings.Contains(got, `"loc":["body","send_window_tz"]`) || !strings.Contains(got, `"type":"value_error"`) {
		t.Fatalf("got %s", got)
	}
}

func TestPutActiveStatusRefused(t *testing.T) {
	_, errs := parseSeqUpdate([]byte(`{"status":"active"}`))
	got := seqErrs(t, errs)
	if !strings.Contains(got, `"loc":["body","status"]`) || !strings.Contains(got, "start") || !strings.Contains(got, `"type":"value_error"`) {
		t.Fatalf("active must be refused with a 422 pointing at start: %s", got)
	}
	for _, s := range []string{"draft", "paused", "completed"} {
		if _, errs := parseSeqUpdate([]byte(`{"status":"` + s + `"}`)); errs != nil {
			t.Errorf("status %q rejected: %s", s, seqErrs(t, errs))
		}
	}
	_, errs = parseSeqUpdate([]byte(`{"status":"bogus"}`))
	if got := seqErrs(t, errs); !strings.Contains(got, `"literal_error"`) {
		t.Fatalf("bogus status: %s", got)
	}
}

func TestUpdateOnlyGivenFields(t *testing.T) {
	u, errs := parseSeqUpdate([]byte(`{"name":"x","description":null,"daily_limit":null,"steps":null}`))
	if errs != nil {
		t.Fatal(seqErrs(t, errs))
	}
	if len(u) != 1 || u["name"] != "x" {
		t.Fatalf("exclude_none: %v", u)
	}
	u, errs = parseSeqUpdate([]byte(`{}`))
	if errs != nil || len(u) != 0 {
		t.Fatalf("empty body: %v %v", u, errs)
	}
	u, errs = parseSeqUpdate([]byte(`{"daily_limit":"5","steps":[],"unknown":1,"send_window_tz":"Asia/Kolkata"}`))
	if errs != nil {
		t.Fatal(seqErrs(t, errs))
	}
	if len(u) != 3 || u["daily_limit"] != int64(5) {
		t.Fatalf("got %v", u)
	}
	if s, ok := u["steps"].(json.RawMessage); !ok || string(s) != "[]" {
		t.Fatalf("steps: %#v", u["steps"])
	}
	_, errs = parseSeqUpdate([]byte(`{"daily_limit":"x"}`))
	want := `[{"type":"int_parsing","loc":["body","daily_limit"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"x"}]`
	if got := seqErrs(t, errs); got != want {
		t.Fatalf("got %s", got)
	}
	if _, errs = parseSeqUpdate([]byte(`[]`)); len(errs) != 1 {
		t.Fatalf("non-object body: %v", errs)
	}
}
