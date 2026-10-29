package sourcehealth

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"math"
	"strconv"
	"time"
)

// isoformat is datetime.isoformat() for a naive datetime.
func isoformat(t *time.Time) any {
	if t == nil {
		return nil
	}
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05")
	}
	return t.Format("2006-01-02T15:04:05.000000")
}

// round3 is Python's round(x, 3): correctly rounded on the exact binary value,
// ties to even (what math.RoundToEven(x*1000)/1000 approximates, minus the
// multiplication error).
func round3(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 3, 64), 64)
	if err != nil {
		return x
	}
	return f
}

// ToAPI is SourceHealth.to_api (key order = Python source order).
func (r *Row) ToAPI() *Obj {
	o := wbcrud.NewObj()
	o.Set("name", r.Name)
	o.Set("state", r.State)
	o.Set("consecutive_zero", r.ConsecutiveZero)
	o.Set("consecutive_nonzero", r.ConsecutiveNonzero)
	if r.LastYield != nil {
		o.Set("last_yield", *r.LastYield)
	} else {
		o.Set("last_yield", nil)
	}
	if r.EwmaYield != nil {
		o.Set("ewma_yield", round3(*r.EwmaYield))
	} else {
		o.Set("ewma_yield", nil)
	}
	o.Set("probes", r.Probes)
	o.Set("last_probe_at", isoformat(r.LastProbeAt))
	o.Set("last_ok_at", isoformat(r.LastOkAt))
	o.Set("last_outage_at", isoformat(r.LastOutageAt))
	o.Set("disabled_at", isoformat(r.DisabledAt))
	if r.DisabledReason != nil {
		o.Set("disabled_reason", *r.DisabledReason)
	} else {
		o.Set("disabled_reason", nil)
	}
	o.Set("manual_override", r.ManualOverride)
	o.Set("updated_at", isoformat(r.UpdatedAt))
	return o
}
