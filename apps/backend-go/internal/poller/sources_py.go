package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// This file holds the pyworker-backed collectors' shared plumbing (decision
// D1) and the hiring/tech collector: JobSpy hiring and tech detection cannot
// run in Go, so it crosses the plan 06 provider RPC as `jobspy_signals`.

// PyCaller runs the provider collectors (provcall.Caller: the in-process rpclocal caller).
type PyCaller interface {
	Call(ctx context.Context, call provcall.Call) (provcall.Result, error)
}

// PyCollectors configures the Python RPC side of the collectors.
type PyCollectors struct {
	// Client is the RPC client (nil: every call fails with provcall.ErrUnavailable).
	Client PyCaller
	// JobSpyMaxJobs is INTENT_POLLER_JOBSPY_MAX_JOBS (<=0: 5).
	JobSpyMaxJobs int
	// JobChangeMaxContactsPerPoll is INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL (<=0: 50).
	JobChangeMaxContactsPerPoll int
	// JobSpyTimeout and SearchTimeout bound one RPC (0: 60s and 20s). A timeout is
	// that collector's failure and is never retried.
	JobSpyTimeout time.Duration
	SearchTimeout time.Duration
}

const (
	defaultJobSpyMaxJobs     = 5
	defaultJobChangeCap      = 50
	defaultJobSpyTimeout     = 60 * time.Second
	defaultSearchTimeout     = 20 * time.Second
	maxCallIDLen             = 200 // ProviderCall.call_id max_length on the Python side
	jobspyProvider           = "jobspy_signals"
	ddgsProvider             = "ddgs_search"
	hiringSurgeWeight        = 7
	newTechWeight            = 6
	maxPyErrorMsgRunes       = 200
	hiringSource             = "jobspy"
	ddgSearchMaxResultsInput = 8
)

func (c PyCollectors) jobSpyMax() int {
	if c.JobSpyMaxJobs > 0 {
		return c.JobSpyMaxJobs
	}
	return defaultJobSpyMaxJobs
}

func (c PyCollectors) jobChangeCap() int {
	if c.JobChangeMaxContactsPerPoll > 0 {
		return c.JobChangeMaxContactsPerPoll
	}
	return defaultJobChangeCap
}

func (c PyCollectors) jobSpyTimeout() time.Duration {
	if c.JobSpyTimeout > 0 {
		return c.JobSpyTimeout
	}
	return defaultJobSpyTimeout
}

func (c PyCollectors) searchTimeout() time.Duration {
	if c.SearchTimeout > 0 {
		return c.SearchTimeout
	}
	return defaultSearchTimeout
}

// PyDeps are the dependencies of the pyworker-backed collectors. The embedded
// SourceDeps carries the clock, the egress client (pricing fetch, SEC) and the
// HTTP-source test seams.
type PyDeps struct {
	SourceDeps
	Py PyCollectors
	// AppEnv and EgressAllowHosts feed the pre-dial URL check of the pricing
	// fetch (egress.CheckURLFor); the client's dial-time check is separate.
	AppEnv           string
	EgressAllowHosts []string
}

// PyWatch is the watch slice the pyworker collectors read: the HTTP sources'
// WatchView plus the tenant, the poll's fire key (call ids), the config and
// the signal types.
type PyWatch struct {
	WatchView
	WorkspaceID string
	FireKey     string
	Config      map[string]any
	SignalTypes []string
}

// ErrPyTimeout is a pyworker call that outlived its deadline (Status "timeout").
var ErrPyTimeout = errors.New("pyworker call timed out")

// ErrNoCompanyName is a hiring watch whose target has no company name to search.
var ErrNoCompanyName = errors.New("watch target has no company name")

// ErrAllLookupsFailed is a job-change poll in which every attempted lookup failed.
var ErrAllLookupsFailed = errors.New("every job change lookup failed")

// PyCallError is a provider failure reported by the Python side (Status
// "error"); Msg is the Python-sourced text, already scrubbed there.
type PyCallError struct{ Msg string }

func (e *PyCallError) Error() string { return "pyworker provider error: " + e.Msg }

// pcCallID is the deterministic RPC call id watch:<id>:<fire_key>:<source>:<n>,
// so a retried poll dedupes at the Python side. Ids over the Python length
// limit are replaced by a digest of the same text (still deterministic).
func pcCallID(w PyWatch, source string, n int) string {
	id := fmt.Sprintf("watch:%s:%s:%s:%d", w.ID, w.FireKey, source, n)
	if len(id) > maxCallIDLen {
		sum := sha256.Sum256([]byte(id))
		id = "watch:" + hex.EncodeToString(sum[:])
	}
	return id
}

// pcCall runs one provider on the Python worker and returns the output's
// `fields`. Failures: provcall.ErrUnavailable/ErrUnauthorized/ErrUnknownProvider,
// ErrPyTimeout, *PyCallError, or the context's error. Nothing is retried here
// beyond the client's own single same-call-id retry of transport errors.
func (c PyCollectors) pcCall(ctx context.Context, w PyWatch, source string, n int, provider string, input map[string]any, timeout time.Duration) (map[string]any, error) {
	if c.Client == nil {
		return nil, provcall.ErrUnavailable
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, &PyCallError{Msg: "invalid input"}
	}
	res, err := c.Client.Call(ctx, provcall.Call{
		CallID: pcCallID(w, source, n), Provider: provider, WorkspaceID: w.WorkspaceID,
		Input: raw, Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}
	if res.Err != nil {
		return nil, res.Err
	}
	switch res.Status {
	case "ok":
	case "timeout":
		return nil, ErrPyTimeout
	default:
		return nil, &PyCallError{Msg: providers.TruncRunes(res.Error, maxPyErrorMsgRunes)}
	}
	v, err := providers.ParseJSON(res.Output)
	if err != nil {
		return nil, &PyCallError{Msg: "unparseable provider output"}
	}
	out, _ := v.(map[string]any)
	if out == nil {
		return nil, &PyCallError{Msg: "provider output is not an object"}
	}
	fields, _ := out["fields"].(map[string]any)
	if fields == nil {
		fields = map[string]any{}
	}
	return fields, nil
}

// isBatchStop reports a failure after which more calls in the same poll are
// pointless or unbounded: the worker is down, rejects us, lacks the provider or
// is hung.
func isBatchStop(err error) bool {
	return errors.Is(err, ErrPyTimeout) || errors.Is(err, provcall.ErrUnavailable) ||
		errors.Is(err, provcall.ErrUnauthorized) || errors.Is(err, provcall.ErrUnknownProvider)
}

// ── Python str helpers ──────────────────────────────────────────────────────

func pcIsCased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
		unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
}

func pcIsCaseIgnorable(r rune) bool {
	switch r {
	case 0x27, 0x2e, 0x3a, 0xb7, 0x387, 0x55f, 0x5f4, 0x2018, 0x2019, 0x2024, 0x2027, 0xfe13, 0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

func pcFinalSigma(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && pcIsCaseIgnorable(rs[j]) {
		j--
	}
	if j < 0 || !pcIsCased(rs[j]) {
		return false
	}
	j = i + 1
	for j < len(rs) && pcIsCaseIgnorable(rs[j]) {
		j++
	}
	return j == len(rs) || !pcIsCased(rs[j])
}

// pcLower is str.lower(): simple lower-casing plus the two special cases
// CPython applies (U+0130 expands to "i" + U+0307; capital sigma is final
// after a cased letter and not before one).
func pcLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range rs {
		switch {
		case r == 0x130:
			b.WriteString("i̇")
		case r == 0x3a3:
			if pcFinalSigma(rs, i) {
				b.WriteRune(0x3c2)
			} else {
				b.WriteRune(0x3c3)
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// pcSpace is the body of a character class equal to Python's \s for str.
const pcSpace = `\t\n\v\f\r\x{1c}-\x{1f} \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// pcI is a case-insensitive "i" as Python's IGNORECASE sees it (it also folds
// U+0130 and U+0131, which RE2 does not).
const pcI = `[iI\x{130}\x{131}]`

// ── hiring + tech collector ─────────────────────────────────────────────────

var bandOrder = []string{"none", "hiring", "growing", "rapid_growth", "hypergrowth"}

// bandRank is _band_rank: unknown or non-string bands rank as "none".
func bandRank(v any) int {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0
	}
	for i, b := range bandOrder {
		if b == s {
			return i
		}
	}
	return 0
}

// isoWeek is _iso_week (UTC).
func isoWeek(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// companyNameFor is _company_name_for: a CIK-only target has no name to search.
func companyNameFor(target string) string {
	t := providers.PyStrip(target)
	if strings.HasPrefix(pcLower(t), "sec_cik:") {
		return ""
	}
	return t
}

// FetchHiringAndTech is sources.fetch_hiring_and_tech: it calls jobspy_signals
// and diffs the returned growth band and tech list against the cursor. A
// pyworker failure (down, error, timeout, bad payload) is an error: the engine
// records a source failure and leaves the cursor untouched.
func FetchHiringAndTech(ctx context.Context, d PyDeps, w PyWatch, wantHiring, wantTech, backfill bool) ([]DetectedEvent, map[string]any, error) {
	signals, err := runJobSpy(ctx, d, w)
	if err != nil {
		return nil, nil, err
	}
	ev, patch := detectHiringTech(w.WatchView, signals, wantHiring, wantTech, backfill, d.now())
	return ev, patch, nil
}

// runJobSpy is _run_jobspy: the analysed signals dict, or an error. "no jobs
// found" is the valid no-op {"growth_signal": "none", "tech_adoption_signal": []}.
func runJobSpy(ctx context.Context, d PyDeps, w PyWatch) (map[string]any, error) {
	noop := func() map[string]any {
		return map[string]any{"growth_signal": "none", "tech_adoption_signal": []any{}}
	}
	company := companyNameFor(w.Target)
	if company == "" {
		return nil, ErrNoCompanyName
	}
	fields, err := d.Py.pcCall(ctx, w, "hiring", 0, jobspyProvider,
		map[string]any{"company": company, "max_jobs": d.Py.jobSpyMax()}, d.Py.jobSpyTimeout())
	if err != nil {
		var pe *PyCallError
		if errors.As(err, &pe) && pe.Msg == "no_jobs_found" {
			return noop(), nil
		}
		return nil, err
	}
	raw, _ := fields["hiring_signals"].(string)
	if raw == "" {
		return noop(), nil
	}
	v, err := providers.ParseJSON([]byte(raw))
	if err != nil {
		return nil, &PyCallError{Msg: "hiring_signals is not valid JSON"}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &PyCallError{Msg: "hiring_signals is not an object"}
	}
	return m, nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// detectHiringTech is the diff half of fetch_hiring_and_tech. hiring_surge
// fires only on an UPWARD band crossing, at most once per ISO week; raw counts
// are never a threshold or dedup input. new_tech_adopted fires on a newly seen
// tech or an intent-tier change.
func detectHiringTech(w WatchView, signals map[string]any, wantHiring, wantTech, backfill bool, now time.Time) ([]DetectedEvent, map[string]any) {
	suppress := w.suppress(backfill)
	cik := w.stableID()
	patch := map[string]any{}
	events := []DetectedEvent{}
	occurred := float64(now.UnixNano()) / 1e9

	if wantHiring {
		newBand, present := signals["growth_signal"]
		if !present {
			newBand = "none"
		}
		prevBand, present := w.Cursor["hiring_band"]
		if !present {
			prevBand = "none"
		}
		prevWeek := w.Cursor["hiring_band_week"]
		week := isoWeek(now)
		crossedUp := bandRank(newBand) > bandRank(prevBand)
		patch["hiring_band"] = newBand
		pw, _ := prevWeek.(string)
		if crossedUp && !suppress && pw != week {
			patch["hiring_band_week"] = week
			events = append(events, DetectedEvent{
				NaturalEventID: cik + ":" + week,
				SignalType:     "hiring_surge",
				Title:          "Hiring surge: " + providers.Str(prevBand) + " → " + providers.Str(newBand),
				Description:    "Growth-band crossed upward to " + providers.Str(newBand) + " (ISO " + week + ")",
				Source:         hiringSource,
				Weight:         hiringSurgeWeight,
				OccurredAt:     occurred,
			})
		} else if crossedUp && suppress {
			patch["hiring_band_week"] = week
		}
	}

	if wantTech {
		known := copyMap(asMap(w.Cursor["known_tech"]))
		sigs, _ := signals["tech_adoption_signal"].([]any)
		for _, e := range sigs {
			sig := asMap(e)
			if sig == nil {
				continue
			}
			rawTech, ok := sig["tech"]
			if !ok {
				rawTech = ""
			}
			techStr, isStr := rawTech.(string)
			if !isStr {
				continue
			}
			tech := NormalizeTech(techStr)
			if tech == "" {
				continue
			}
			intent := "mentions"
			if iv := sig["intent"]; providers.Truthy(iv) {
				intent = pcLower(providers.Str(iv))
			}
			if prev, ok := known[tech].(string); ok && prev == intent {
				continue
			}
			known[tech] = intent
			if !suppress {
				category := ""
				if cv, ok := sig["category"]; ok {
					category = providers.Str(cv)
				}
				events = append(events, DetectedEvent{
					NaturalEventID: cik + ":" + tech + ":" + intent,
					SignalType:     "new_tech_adopted",
					Title:          "Adopting " + techStr + " (" + intent + ")",
					Description:    category + ": " + techStr + " intent=" + intent,
					Source:         hiringSource,
					Weight:         newTechWeight,
					OccurredAt:     occurred,
				})
			}
		}
		patch["known_tech"] = known
	}
	return events, patch
}
