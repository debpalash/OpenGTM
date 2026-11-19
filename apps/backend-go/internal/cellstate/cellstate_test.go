package cellstate

import "testing"

func TestOfCell(t *testing.T) {
	long := "No LLM provider is configured: add a key in Settings"
	cases := []struct {
		name, status, errText string
		want                  Outcome
	}{
		{"value", "complete", "", Filled},
		{"complete with a stale error text", "complete", "timeout", Filled},
		{"providers ran, found nothing", "error", "no_data", NoData},
		{"empty error", "error", "", NoData},
		{"research answered nothing", "error", "no_answer", NoData},
		{"verify has no address", "error", "no_email_to_verify: the Email column has no value in this row", NoData},
		{"input column had no data", "error", "upstream_dependency_failed", NoData},
		{"no LLM provider", "error", long, NeedsSetup},
		{"browser service down", "error", "browser_service_unavailable: deep_scraper needs the browser service, which could not be reached; check that it is running", NeedsSetup},
		{"nothing selected", "error", "no_providers_selected", NeedsSetup},
		{"smtp blocked", "error", "smtp_port_blocked: SMTP port 25 is blocked on this server, so addresses cannot be verified; use a verification provider key", NeedsSetup},
		{"timeout", "error", "timeout", Error},
		{"rate limited", "error", "rate_limited: ddg_company answered with a rate limit and is paused for a few minutes", Error},
		{"cooling down", "error", "providers_unavailable: wikidata (cooldown) - provider cooling down after a rate limit or repeated timeouts", Error},
		{"crash", "error", "provider crashed", Error},
		{"http 500", "error", "HTTP 500", Error},
		{"skipped", "skipped", "", Skipped},
	}
	for _, c := range cases {
		if got := OfCell(c.status, c.errText); got != c.want {
			t.Errorf("%s: OfCell(%q, %q) = %q, want %q", c.name, c.status, c.errText, got, c.want)
		}
	}
}

func TestRunStatus(t *testing.T) {
	cases := []struct {
		name               string
		c                  Counts
		runFailed, stopped bool
		want               string
	}{
		{"all filled", Counts{Filled: 10}, false, false, StatusComplete},
		{"filled and no data", Counts{Filled: 5, NoData: 5}, false, false, StatusComplete},
		{"needs setup is not an error", Counts{Filled: 5, NoData: 2, NeedsSetup: 3}, false, false, StatusComplete},
		{"nothing found anywhere", Counts{NoData: 10}, false, false, StatusComplete},
		{"some errors", Counts{Filled: 5, Errors: 1}, false, false, StatusCompleteWithErrors},
		{"errors among no data", Counts{NoData: 4, Errors: 1}, false, false, StatusCompleteWithErrors},
		{"every attempted cell errored", Counts{Errors: 6}, false, false, StatusFailed},
		{"the run itself failed", Counts{Filled: 5}, true, false, StatusFailed},
		{"stopped", Counts{Filled: 1, Errors: 4}, false, true, StatusPaused},
		{"nothing attempted", Counts{}, false, false, StatusComplete},
	}
	for _, c := range cases {
		if got := RunStatus(c.c, c.runFailed, c.stopped); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
