// Package certification ports apps/api/services/integrations/certification.py:
// the fail-closed maturity registry. A certificate counts only when its HMAC
// attestation, evidence contract, validity window and build sha all hold.
// Nothing here logs or returns the key.
package certification

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

const (
	PathEnv     = "OPENGTM_INTEGRATION_CERTIFICATIONS"
	KeyEnv      = "OPENGTM_INTEGRATION_CERTIFICATION_KEY"
	BuildSHAEnv = "OPENGTM_BUILD_SHA"

	// MaxCertificationLifetimeDays is MAX_CERTIFICATION_LIFETIME_DAYS.
	MaxCertificationLifetimeDays = 92

	// maxFileBytes bounds the certification file (hardening; Python reads it all).
	maxFileBytes = 8 << 20
)

// Options mirrors the keyword arguments of the Python functions. Empty Path,
// Key and BuildSHA read the three environment names; a nil Now is the clock.
// Pass config.Flags.CertificationKey.Reveal() as Key at the call site only.
type Options struct {
	Path, Key, BuildSHA string
	Now                 func() time.Time
}

func (o Options) resolved() (path, key, build string, now time.Time) {
	path, key, build = o.Path, o.Key, o.BuildSHA
	if path == "" {
		path = os.Getenv(PathEnv)
	}
	if key == "" {
		key = os.Getenv(KeyEnv)
	}
	if build == "" {
		build = os.Getenv(BuildSHAEnv)
	}
	if o.Now != nil {
		now = o.Now()
	} else {
		now = time.Now()
	}
	return path, key, build, now.UTC()
}

// loadCertificates reads the file; anything unreadable, oversized, malformed
// or not a JSON list yields no certificates and never an error.
func loadCertificates(path string) []any {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil || len(raw) > maxFileBytes {
		return nil
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		return nil
	}
	list, _ := v.([]any)
	return list
}

func (o Options) loadValid(requested map[string]bool, builds map[string]string) map[string]map[string]any {
	path, key, build, now := o.resolved()
	return validBySubject(loadCertificates(path), key, now, build, requested, builds)
}

// StatusFor is certification_statuses for a bounded runtime catalog (for
// example installed connectors). builds maps connector subjects to the
// package sha256 the certificate must name. Python raises on an unknown
// subject; Go omits it from the result (it is never certified) - use Known to
// detect it.
func StatusFor(subjects []string, builds map[string]string, o Options) map[string]Status {
	requested := make(map[string]bool, len(subjects))
	for _, s := range subjects {
		requested[s] = true
	}
	valid := o.loadValid(requested, builds)
	out := make(map[string]Status, len(subjects))
	for _, id := range subjects {
		if !Known(id) {
			continue
		}
		c := projection(valid[id])
		out[id] = Status{RequiredChecks: RequiredChecks(id), Maturity: maturity(c), Certification: c}
	}
	return out
}

func projection(cert map[string]any) *Certification {
	if cert == nil {
		return nil
	}
	c := &Certification{
		ValidatedAt: cert["validated_at"], ExpiresAt: cert["expires_at"], BuildSHA: cert["build_sha"],
		ValidationRunID: cert["validation_run_id"], EvidenceURL: cert["evidence_url"],
		EvidenceSHA256: cert["evidence_sha256"], Mode: cert["mode"],
	}
	if strings.HasPrefix(subjectID(cert), "connector:") {
		c.SubjectBuildSHA256 = cert["subject_build_sha256"]
	}
	return c
}

// ---- Python value helpers -------------------------------------------------

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr is str(v) for the JSON types; containers render a marker that can
// never equal a subject id, sha or url.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		s, err := canonjson.FloatRepr(x)
		if err != nil {
			return "nan"
		}
		return s
	}
	return "<object>"
}

func pyStrOr(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
}

func subjectID(c map[string]any) string {
	if truthy(c["subject_id"]) {
		return pyStr(c["subject_id"])
	}
	return pyStrOr(c["integration_id"])
}

func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// ---- time -----------------------------------------------------------------

var isoRE = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2})(?::(\d{2})(?::(\d{2})(?:[.,](\d+))?)?)?(Z|[+-]\d{2}(?::?\d{2}(?::?\d{2}(?:\.\d+)?)?)?)?)?$`)

// parseTime is _parse_time: datetime.fromisoformat(value with Z -> +00:00)
// converted to UTC. Naive values are read as UTC (Python reads them in the
// process-local zone; the recorded fixtures run under TZ=UTC). Only the ISO
// extended forms are accepted; non-strings never parse.
func parseTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	m := isoRE.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	n := func(i int) int { x, _ := strconv.Atoi(m[i]); return x }
	year, month, day := n(1), n(2), n(3)
	hour, minute, sec, nsec := 0, 0, 0, 0
	if m[4] != "" {
		hour = n(4)
	}
	if m[5] != "" {
		minute = n(5)
	}
	if m[6] != "" {
		sec = n(6)
	}
	if m[7] != "" {
		frac := m[7]
		if len(frac) > 6 {
			frac = frac[:6]
		}
		frac += strings.Repeat("0", 6-len(frac))
		nsec = n2(frac) * 1000
	}
	if year < 1 || hour > 23 || minute > 59 || sec > 59 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, nsec, time.UTC)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, false
	}
	off := 0
	if z := m[8]; z != "" && z != "Z" {
		sign := 1
		if z[0] == '-' {
			sign = -1
		}
		digits := strings.NewReplacer(":", "").Replace(z[1:])
		if i := strings.IndexByte(digits, '.'); i >= 0 {
			digits = digits[:i]
		}
		h, _ := strconv.Atoi(digits[:2])
		mi, se := 0, 0
		if len(digits) >= 4 {
			mi, _ = strconv.Atoi(digits[2:4])
		}
		if len(digits) >= 6 {
			se, _ = strconv.Atoi(digits[4:6])
		}
		if h > 23 || mi > 59 || se > 59 {
			return time.Time{}, false
		}
		off = sign * (h*3600 + mi*60 + se)
	}
	return t.Add(-time.Duration(off) * time.Second), true
}

func n2(s string) int { x, _ := strconv.Atoi(s); return x }

// ---- urlsplit -------------------------------------------------------------

// httpsEvidence reports Python's check scheme == "https" and netloc non-empty
// and no userinfo (urlsplit semantics, including its stripping of leading
// control characters and removal of tab/CR/LF).
func httpsEvidence(raw string) bool {
	raw = strings.TrimLeftFunc(raw, func(r rune) bool { return r <= 0x20 })
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	i := strings.IndexByte(raw, ':')
	if i <= 0 || !strings.EqualFold(raw[:i], "https") {
		return false
	}
	rest := raw[i+1:]
	if !strings.HasPrefix(rest, "//") {
		return false
	}
	rest = rest[2:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return rest != "" && !strings.Contains(rest, "@")
}

// ---- payload and attestation ---------------------------------------------

// payload is _payload: json.dumps(unsigned, sort_keys=True,
// separators=(",", ":"), ensure_ascii=False).encode("utf-8"). canonjson only
// renders ensure_ascii=True (different bytes for non-ASCII text), so this
// encoder differs in string escaping only and reuses canonjson.FloatRepr.
func payload(cert map[string]any) ([]byte, error) {
	unsigned := make(map[string]any, len(cert))
	for k, v := range cert {
		if k != "attestation" {
			unsigned[k] = v
		}
	}
	var b strings.Builder
	if err := writeValue(&b, unsigned); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeValue(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		if !utf8.ValidString(x) {
			return errors.New("invalid UTF-8")
		}
		writeString(b, x)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("non-finite number")
		}
		s, _ := canonjson.FloatRepr(x)
		b.WriteString(s)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeValue(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // UTF-8 byte order == code point order
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if !utf8.ValidString(k) {
				return errors.New("invalid UTF-8")
			}
			writeString(b, k)
			b.WriteByte(':')
			if err := writeValue(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported type %T", v)
	}
	return nil
}

func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

func sign(key string, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func eq(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// Attest is attest_certificate: it returns a copy of cert carrying the
// hmac-sha256 attestation. Error strings equal the Python ValueErrors.
func Attest(cert map[string]any, key string) (map[string]any, error) {
	if key == "" {
		return nil, errors.New("certification key must not be empty")
	}
	if !Known(subjectID(cert)) {
		return nil, errors.New("certificate subject is unknown")
	}
	if !evidenceContractValid(cert) {
		return nil, errors.New("certificate does not satisfy the controlled-live evidence contract")
	}
	result := make(map[string]any, len(cert)+1)
	for k, v := range cert {
		result[k] = v
	}
	body, err := payload(result)
	if err != nil {
		return nil, fmt.Errorf("certificate is not serialisable: %w", err)
	}
	result["attestation"] = map[string]any{
		"algorithm": "hmac-sha256",
		"key_id":    keyID(key),
		"signature": sign(key, body),
	}
	return result, nil
}

var (
	runIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// evidenceContractValid is _evidence_contract_valid.
func evidenceContractValid(c map[string]any) bool {
	id := subjectID(c)
	legacy, explicit := pyStrOr(c["integration_id"]), pyStrOr(c["subject_id"])
	if legacy != "" && explicit != "" && legacy != explicit {
		return false
	}
	runID := pyStrOr(c["validation_run_id"])
	build := pyStrOr(c["build_sha"])
	validated, okV := parseTime(c["validated_at"])
	expires, okE := parseTime(c["expires_at"])
	if c["status"] != "supported" || !runIDRE.MatchString(runID) ||
		pyStrip(build) == "" || strings.IndexFunc(build, isPySpace) >= 0 ||
		!okV || !okE || !validated.Before(expires) ||
		expires.Sub(validated) > MaxCertificationLifetimeDays*24*time.Hour ||
		!httpsEvidence(pyStrOr(c["evidence_url"])) {
		return false
	}
	if strings.HasPrefix(id, "connector:") && !hex64RE.MatchString(pyStrOr(c["subject_build_sha256"])) {
		return false
	}
	required := requiredSet(id)
	checks, isMap := c["checks"].(map[string]any)
	if c["mode"] != "controlled_live" || len(required) == 0 || !isMap {
		return false
	}
	if !hex64RE.MatchString(pyStrOr(c["external_system_id_hash"])) || !hex64RE.MatchString(pyStrOr(c["evidence_sha256"])) {
		return false
	}
	for name := range required {
		check, ok := checks[name].(map[string]any)
		if !ok || check["passed"] != true || pyStrip(pyStrOr(check["evidence"])) == "" {
			return false
		}
	}
	return true
}

// valid is _valid: signature, key id, build sha and the validity window.
func valid(c map[string]any, key string, now time.Time, buildSHA string) bool {
	att, ok := c["attestation"].(map[string]any)
	if key == "" || !ok {
		return false
	}
	body, err := payload(c)
	if err != nil {
		return false
	}
	validated, okV := parseTime(c["validated_at"])
	expires, okE := parseTime(c["expires_at"])
	return Known(subjectID(c)) &&
		evidenceContractValid(c) &&
		c["status"] == "supported" &&
		buildSHA != "" &&
		eq(pyStrOr(c["build_sha"]), buildSHA) &&
		truthy(c["validation_run_id"]) &&
		okV && okE &&
		!validated.After(now) && now.Before(expires) &&
		httpsEvidence(pyStrOr(c["evidence_url"])) &&
		att["algorithm"] == "hmac-sha256" &&
		eq(pyStrOr(att["key_id"]), keyID(key)) &&
		eq(pyStrOr(att["signature"]), sign(key, body))
}

// validBySubject resolves only unambiguous valid certificates, independent of
// file order. requested nil means every subject.
func validBySubject(certs []any, key string, now time.Time, buildSHA string,
	requested map[string]bool, builds map[string]string) map[string]map[string]any {
	out := map[string]map[string]any{}
	ambiguous := map[string]bool{}
	for _, item := range certs {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := subjectID(c)
		if requested != nil && !requested[id] {
			continue
		}
		if !valid(c, key, now, buildSHA) {
			continue
		}
		if strings.HasPrefix(id, "connector:") {
			want := builds[id]
			if want == "" || !eq(pyStrOr(c["subject_build_sha256"]), want) {
				continue
			}
		}
		if _, dup := out[id]; dup || ambiguous[id] {
			delete(out, id)
			ambiguous[id] = true
			continue
		}
		out[id] = c
	}
	return out
}
