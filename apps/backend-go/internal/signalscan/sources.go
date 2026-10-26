package signalscan

import (
	"bytes"
	"encoding/json"
	"sort"
)

// signalSource is one SIGNAL_SOURCES entry (integrations/certification.py), in Python's order.
type signalSource struct {
	ID          string
	SignalTypes []string
	Specialized []string // SIGNAL_SPECIALIZED_CHECKS
}

// signalChecks is SIGNAL_CHECKS.
var signalChecks = []string{"external_read", "provenance", "deduplication", "failure_recovery", "tenant_isolation"}

var signalSources = []signalSource{
	{"jobspy", []string{"hiring", "partnership_hiring"}, []string{"employment_signal_normalization"}},
	{"sec_edgar", []string{"funding", "leadership_change"}, []string{"cik_resolution", "filing_cursor"}},
	{"website_monitor", []string{"website_change", "pricing_page_change"}, []string{"content_change_detection"}},
	{"tech_stack", []string{"tech_change", "new_tech_adopted"}, []string{"technology_fingerprint"}},
	{"news_search", []string{"news", "funding"}, []string{"source_attribution"}},
}

// Certification is the evidence of a validated source (the fields signal_source_catalog copies).
type Certification struct {
	ValidatedAt, ExpiresAt, BuildSHA, ValidationRunID, EvidenceURL, EvidenceSHA256, Mode string
}

// Certifier answers whether a source has a currently valid, attested controlled-live
// certificate. nil means none is configured: every source is "beta" with certification null
// (the fail-closed default of signal_source_catalog).
type Certifier interface {
	ValidSource(id string) (Certification, bool)
}

// requiredChecks is sorted(SIGNAL_CHECKS | SIGNAL_SPECIALIZED_CHECKS[id]).
func (s signalSource) requiredChecks() []string {
	out := append(append([]string{}, signalChecks...), s.Specialized...)
	sort.Strings(out)
	return out
}

func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSpace(buf.Bytes())
}

// CatalogJSON renders GET /api/signals/sources: {"sources":[{id, signal_types,
// required_checks, maturity, certification}, ...]} in Python's key order.
func CatalogJSON(c Certifier) []byte {
	var b bytes.Buffer
	b.WriteString(`{"sources":[`)
	for i, s := range signalSources {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":`)
		b.Write(marshal(s.ID))
		b.WriteString(`,"signal_types":`)
		b.Write(marshal(s.SignalTypes))
		b.WriteString(`,"required_checks":`)
		b.Write(marshal(s.requiredChecks()))
		var cert Certification
		ok := false
		if c != nil {
			cert, ok = c.ValidSource(s.ID)
		}
		if !ok {
			b.WriteString(`,"maturity":"beta","certification":null}`)
			continue
		}
		b.WriteString(`,"maturity":"supported","certification":{`)
		for j, kv := range [][2]string{
			{"validated_at", cert.ValidatedAt}, {"expires_at", cert.ExpiresAt}, {"build_sha", cert.BuildSHA},
			{"validation_run_id", cert.ValidationRunID}, {"evidence_url", cert.EvidenceURL},
			{"evidence_sha256", cert.EvidenceSHA256}, {"mode", cert.Mode},
		} {
			if j > 0 {
				b.WriteByte(',')
			}
			b.Write(marshal(kv[0]))
			b.WriteByte(':')
			b.Write(marshal(kv[1]))
		}
		b.WriteString(`}}`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}
