// Package sourcehealth ports services/leadgen/source_health.py: the global
// (non-tenant, non-RLS) per-source active-probe health ledger and its
// healthy/degraded/auto_disabled state machine.
package sourcehealth

import (
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Obj is the insertion-ordered JSON object (key order = Python dict order).
type Obj = wbcrud.Obj

// EWMAAlpha is EWMA_ALPHA (dumped from Python into testdata/transition_vectors.json).
const EWMAAlpha = 0.3

// DefaultOutageRatio is SOURCE_HEALTH_OUTAGE_RATIO's default.
const DefaultOutageRatio = 0.6

// States.
const (
	StateHealthy  = "healthy"
	StateDegraded = "degraded"
	StateDisabled = "auto_disabled"
)

// Thresholds are the consecutive-zero / consecutive-nonzero counts that drive
// the state machine.
type Thresholds struct{ Degrade, Disable, Recover int }

// DefaultThresholds are the SOURCE_HEALTH_{DEGRADE,DISABLE,RECOVER}_THRESHOLD defaults.
var DefaultThresholds = Thresholds{Degrade: 3, Disable: 6, Recover: 2}

// ThresholdsFromEnv reads the three knobs; unset or unparsable values keep the
// default (pydantic would reject the latter at startup; the Go side is lenient).
func ThresholdsFromEnv(get func(string) string) Thresholds {
	th := DefaultThresholds
	for _, k := range []struct {
		dst *int
		key string
	}{
		{&th.Degrade, "SOURCE_HEALTH_DEGRADE_THRESHOLD"},
		{&th.Disable, "SOURCE_HEALTH_DISABLE_THRESHOLD"},
		{&th.Recover, "SOURCE_HEALTH_RECOVER_THRESHOLD"},
	} {
		if n, err := strconv.Atoi(strings.TrimSpace(get(k.key))); err == nil {
			*k.dst = n
		}
	}
	return th
}

// Row is one source_health row. Counters are never NULL in the table (a Python
// None counter is treated as 0, which is the Go zero value).
type Row struct {
	Name                                string
	State                               string
	ConsecutiveZero, ConsecutiveNonzero int
	LastYield                           *int
	EwmaYield                           *float64
	Probes                              int
	LastProbeAt, LastOkAt               *time.Time
	LastOutageAt, DisabledAt            *time.Time
	DisabledReason                      *string
	ManualOverride                      bool
	UpdatedAt                           *time.Time // read-only: set by the database
}

// ApplyTransition is _apply_transition: fold one probe result into the row.
// Always records the observational fields; on an outage run a zero yield only
// stamps last_outage_at; a nonzero yield advances recovery even during an outage.
func ApplyTransition(r *Row, probeYield int, outage bool, now time.Time, th Thresholds) {
	now = naive(now)
	r.LastProbeAt = &now
	y := probeYield
	r.LastYield = &y
	r.Probes++
	if r.EwmaYield == nil {
		v := float64(probeYield)
		r.EwmaYield = &v
	} else {
		// float64() conversions forbid fused multiply-add so the result is
		// bit-identical to CPython's separately rounded operations.
		v := float64(EWMAAlpha*float64(probeYield)) + float64((1.0-EWMAAlpha)**r.EwmaYield)
		r.EwmaYield = &v
	}

	if probeYield > 0 {
		r.ConsecutiveZero = 0
		r.ConsecutiveNonzero++
		ok := now
		r.LastOkAt = &ok
		switch r.State {
		case StateDisabled:
			if r.ConsecutiveNonzero >= th.Recover {
				r.State = StateHealthy
				r.DisabledAt = nil
				r.DisabledReason = nil
			}
		case StateDegraded:
			r.State = StateHealthy
		}
		return
	}

	if outage {
		o := now
		r.LastOutageAt = &o
		return
	}

	r.ConsecutiveNonzero = 0
	r.ConsecutiveZero++
	if r.ConsecutiveZero >= th.Disable && !r.ManualOverride {
		r.State = StateDisabled
		d := now
		r.DisabledAt = &d
		reason := "zero_yield"
		r.DisabledReason = &reason
	} else if r.ConsecutiveZero >= th.Degrade {
		if r.State != StateDisabled {
			r.State = StateDegraded
		}
	}
}

// OutageGuard is phase 2 of handle_source_health_check: zeros/max(len,1) >= ratio.
func OutageGuard(yields []int, ratio float64) bool {
	total := len(yields)
	if total == 0 {
		total = 1
	}
	zeros := 0
	for _, y := range yields {
		if y == 0 {
			zeros++
		}
	}
	return float64(zeros)/float64(total) >= ratio
}

// naive converts to naive UTC at microsecond precision (timestamp without time zone).
func naive(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
