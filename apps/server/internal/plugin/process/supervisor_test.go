//go:build unix

package process

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRunSuccessMergesHostEvidence(t *testing.T) {
	s := newSup(t, Options{Client: testClient(t, okTransport())})
	p := pspec{network: []string{"https://api.example.com"}, extra: "cost_per_lookup: 0.02\ncapability: company_size"}.build(t)

	out := mustRun(t, s, p, map[string]any{"mode": "echo", "value": "hi"}, nil)
	if got := fieldsOf(t, out)["echo"]; got != "hi" {
		t.Fatalf("fields: %v", out.Records[0].Fields)
	}
	ev := out.Records[0].Evidence
	if ev["runtime"] != "process" || asMap(t, ev["plugin"])["note"] != "from plugin" {
		t.Fatalf("evidence: %v", ev)
	}
	// A provider that reports no cost is charged the manifest's cost_per_lookup.
	if out.CostUSD != 0.02 {
		t.Fatalf("cost: %v", out.CostUSD)
	}
	if out.SDK.Name != "raw" {
		t.Fatalf("sdk: %+v", out.SDK)
	}
	st := s.Stats()
	if st.Succeeded != 1 || st.Running != 0 || st.Started != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestRecordsStreamingAndResultFields(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{kind: "scraper"}.build(t)
	out := mustRun(t, s, p, map[string]any{"mode": "records_and_result"}, nil)
	if len(out.Records) != 3 || out.Pages != 7 || out.CostUSD != 0.5 || out.Stopped != "max_pages" {
		t.Fatalf("outcome: %+v", out)
	}
	for i, r := range out.Records {
		if r.Fields["n"] != float64(i+1) {
			t.Fatalf("record %d out of order: %v", i, r.Fields)
		}
	}
}

func TestManyRecordsAcrossFrames(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{kind: "scraper"}.build(t)
	out := mustRun(t, s, p, map[string]any{"mode": "many_records", "n": 2500, "pad": 200}, nil)
	if len(out.Records) != 2500 {
		t.Fatalf("got %d records", len(out.Records))
	}
}

func TestOutputLimits(t *testing.T) {
	s := newSup(t, Options{MaxRecords: 100})
	p := pspec{kind: "scraper"}.build(t)
	_, err := run(t, s, p, map[string]any{"mode": "many_records", "n": 500}, nil)
	wantError(t, err, CodeOutputTooLarge, false)

	s2 := newSup(t, Options{MaxOutputBytes: 50_000})
	_, err = run(t, s2, p, map[string]any{"mode": "many_records", "n": 1000, "pad": 500}, nil)
	wantError(t, err, CodeOutputTooLarge, false)
}

func TestOutputsSchemaIsEnforced(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{outputs: `{type: object, properties: {n: {type: integer}}, required: [n], additionalProperties: false}`}.build(t)

	mustRun(t, s, p, map[string]any{"mode": "bad_output", "fields": map[string]any{"n": 1}}, nil)
	_, err := run(t, s, p, map[string]any{"mode": "bad_output", "fields": map[string]any{"n": "one"}}, nil)
	pe := wantError(t, err, CodeInvalidOutput, false)
	if !strings.Contains(pe.Message, "outputs schema") {
		t.Fatalf("message: %s", pe.Message)
	}
	_, err = run(t, s, p, map[string]any{"mode": "bad_output", "fields": map[string]any{"n": 1, "extra": true}}, nil)
	wantError(t, err, CodeInvalidOutput, false)
}

func TestRecordShapeIsEnforced(t *testing.T) {
	s := newSup(t, Options{})
	_, err := run(t, s, pspec{}.build(t), map[string]any{"mode": "no_fields"}, nil)
	wantError(t, err, CodeInvalidOutput, false)
	// A provider answers with at most one record.
	_, err = run(t, s, pspec{kind: "provider"}.build(t), map[string]any{"mode": "records_and_result"}, nil)
	wantError(t, err, CodeInvalidOutput, false)
}

func TestFailureSemantics(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{}.build(t)
	yes, no := true, false
	cases := []struct {
		name      string
		code      string
		retryable *bool
		want      bool
	}{
		{"upstream retries by default", CodeUpstream, nil, true},
		{"rate limit retries by default", CodeRateLimited, nil, true},
		{"timeout retries by default", CodeTimeout, nil, true},
		{"invalid input is permanent", CodeInvalidInput, nil, false},
		{"auth failure is permanent", CodeAuthFailed, nil, false},
		{"plugin exception is permanent", CodePluginException, nil, false},
		{"unknown code is permanent", "vendor_specific", nil, false},
		{"plugin can mark permanent", CodeUpstream, &no, false},
		{"plugin can mark retryable", "vendor_specific", &yes, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := map[string]any{"mode": "fail", "code": c.code, "message": "boom"}
			if c.retryable != nil {
				in["retryable"] = *c.retryable
			}
			_, err := run(t, s, p, in, nil)
			pe := wantError(t, err, c.code, c.want)
			if !pe.FromPlugin || pe.Message != "boom" {
				t.Fatalf("error: %+v", pe)
			}
		})
	}
}

func TestCrashesAreRetryableAndDiagnosed(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{}.build(t)

	_, err := run(t, s, p, map[string]any{"mode": "segv"}, nil)
	pe := wantError(t, err, CodeCrashed, true)
	if !strings.Contains(pe.Message, "signal") || pe.FromPlugin {
		t.Fatalf("segv: %+v", pe)
	}

	_, err = run(t, s, p, map[string]any{"mode": "sigkill_self"}, nil)
	if pe := wantError(t, err, CodeCrashed, true); !strings.Contains(pe.Message, "killed") {
		t.Fatalf("sigkill: %+v", pe)
	}

	_, err = run(t, s, p, map[string]any{"mode": "exit1"}, nil)
	pe = wantError(t, err, CodeCrashed, true)
	if !strings.Contains(pe.Message, "status 1") || !strings.Contains(pe.Stderr, "fatal: something broke") {
		t.Fatalf("exit1: %+v", pe)
	}

	_, err = run(t, s, p, map[string]any{"mode": "exit0"}, nil)
	wantError(t, err, CodeCrashed, true)

	st := s.Stats()
	if st.Crashed != 4 || st.Failed != 4 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestMissingSDKGivesAHint(t *testing.T) {
	// No PythonPath: the SDK import fails and the error says what to do.
	s := newSup(t, Options{PythonPath: []string{t.TempDir()}})
	p := pspec{script: "sdk_provider.py", extra: ""}.build(t)
	_, err := run(t, s, p, map[string]any{}, nil)
	pe := wantError(t, err, CodeCrashed, true)
	if !strings.Contains(pe.Message, "opengtm_sdk") || !strings.Contains(pe.Message, "OPENGTM_PLUGIN_PYTHONPATH") {
		t.Fatalf("no hint: %s", pe.Message)
	}
}

func TestProtocolViolationsAreRejected(t *testing.T) {
	s := newSup(t, Options{MaxFrameBytes: 1 << 20, HandshakeTimeout: 5 * time.Second})
	p := pspec{}.build(t)
	for _, mode := range []string{"garbage", "array_frame", "unknown_type", "second_hello", "oversize", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			_, err := run(t, s, p, map[string]any{"mode": mode}, nil)
			pe, ok := err.(*Error)
			if !ok {
				t.Fatalf("want *Error, got %v", err)
			}
			// A truncated frame followed by exit looks like a crash; every
			// other violation is a permanent protocol error.
			if mode == "truncated" {
				if pe.Code != CodeProtocol && pe.Code != CodeCrashed {
					t.Fatalf("%s: %+v", mode, pe)
				}
				return
			}
			wantError(t, err, CodeProtocol, false)
		})
	}
}

func TestProtocolVersionMismatch(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{}.build(t)
	s.opts.PassEnv = []string{"RAW_HELLO_PROTOCOL"}
	t.Setenv("RAW_HELLO_PROTOCOL", "99")
	_, err := run(t, s, p, map[string]any{}, nil)
	pe := wantError(t, err, CodeProtocol, false)
	if !strings.Contains(pe.Message, "[99]") {
		t.Fatalf("message: %s", pe.Message)
	}
}

func TestStartupFailures(t *testing.T) {
	s := newSup(t, Options{})
	// Command not found is permanent: retrying cannot fix a missing binary.
	p := pspec{command: "[definitely-not-installed-xyz, a.py]"}.build(t)
	_, err := run(t, s, p, map[string]any{}, nil)
	wantError(t, err, CodeLaunch, false)

	// A relative command may not escape the plugin directory.
	p = pspec{command: "[../evil.sh]"}.build(t)
	_, err = run(t, s, p, map[string]any{}, nil)
	wantError(t, err, CodeLaunch, false)

	// A command that starts but never speaks is a (retryable) crash.
	s2 := newSup(t, Options{HandshakeTimeout: 300 * time.Millisecond})
	p = pspec{command: "[python3, -c, 'import time; time.sleep(60)']"}.build(t)
	start := time.Now()
	_, err = run(t, s2, p, map[string]any{}, nil)
	wantError(t, err, CodeCrashed, true)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("handshake timeout took %s", time.Since(start))
	}
}

func TestProgressIsRateLimitedAndLogsAreBounded(t *testing.T) {
	log := &captureLog{}
	s := newSup(t, Options{Logger: log})
	p := pspec{}.build(t)
	var n int
	_, err := s.Run(context.Background(), p, Request{Inputs: map[string]any{"mode": "progress"}, OnProgress: func(Progress) { n++ }})
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 || n > 5 {
		t.Fatalf("progress callbacks: %d (50 frames sent in a burst)", n)
	}
	mustRun(t, s, p, map[string]any{"mode": "logspam"}, nil)
	if got := strings.Count(log.String(), "line "); got > maxLogLines {
		t.Fatalf("%d log lines kept, limit %d", got, maxLogLines)
	}
}

func TestCrashDoesNotAffectOtherRuns(t *testing.T) {
	s := newSup(t, Options{MaxProcesses: 4})
	p := pspec{}.build(t)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		mode := "ok"
		if i%2 == 0 {
			mode = "segv"
		}
		go func() {
			_, err := run(t, s, p, map[string]any{"mode": mode}, nil)
			if (mode == "ok") != (err == nil) {
				errs <- fmt.Errorf("mode %s: %v", mode, err)
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
