// Package signalscan ports the Python signal scanner (services/signals/monitor.py
// run_signal_scan, store.add_signal, workbook/refresh.py handle_signal_scan):
// deterministic scan ids so a Go and a Python scan racing in one period insert
// each signal once, the hiring detector (through the Python worker), score
// boosts and the workbook trigger pass.
package signalscan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// SchemaVersion is SCAN_KEY_SCHEMA_VERSION: part of every scan id.
const SchemaVersion = 1

// HiringBand is monitor._hiring_band: a coarse open-positions band, so small
// count jitter between scans does not churn the dedup key.
func HiringBand(total int64) string {
	switch {
	case total >= 51:
		return "surge"
	case total >= 21:
		return "high"
	case total >= 6:
		return "moderate"
	}
	return "low"
}

// PeriodBucket is monitor._period_bucket: the ISO year-week of ts in UTC, e.g.
// "2026-W35". Re-scans within one week dedup to one signal.
func PeriodBucket(ts time.Time) string {
	y, w := ts.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// ScanSignalID is monitor.scan_signal_id:
//
//	sha256("scan|v1|<ws>|<lead_id>|<signal_type>|<natural_key>")
//
// A non-zero ts appends "|<period bucket of ts>" to naturalKey (Python callers
// build "<band>|<bucket>" themselves); a zero ts uses naturalKey verbatim.
func ScanSignalID(workspaceID string, leadID int64, signalType, naturalKey string, ts time.Time) string {
	if !ts.IsZero() {
		naturalKey += "|" + PeriodBucket(ts)
	}
	raw := "scan|v" + strconv.Itoa(SchemaVersion) + "|" + workspaceID + "|" + strconv.FormatInt(leadID, 10) + "|" + signalType + "|" + naturalKey
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
