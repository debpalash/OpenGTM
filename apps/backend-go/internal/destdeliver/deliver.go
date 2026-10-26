package destdeliver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// Destination is the slice of a destination row that delivery needs (the store type lands in
// plan 10 T7; this stays decoupled from it).
type Destination struct {
	ID          string
	WorkspaceID string
	AudienceID  string
	Type        string // exact, case-sensitive (Python compared the stored string)
	Config      map[string]any
	FieldMap    map[string]any
}

// Deliverer delivers one lead snapshot to one destination through the provider Ports. It makes
// no network calls itself: webhook, Slack, campaign, sheet and Airtable traffic all goes
// through the ports, whose HTTP clients use the SSRF-guarded egress dialer.
type Deliverer struct {
	ports   Ports
	timeout time.Duration
}

// DefaultTimeout is used when New is given a non-positive timeout (DEST_DELIVERY_TIMEOUT default).
const DefaultTimeout = 30 * time.Second

const (
	maxErrorChars = 500
	redacted      = "[redacted]"
)

// New returns a Deliverer; timeout bounds each Deliver call (DEST_DELIVERY_TIMEOUT).
func New(ports Ports, timeout time.Duration) *Deliverer {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Deliverer{ports: ports, timeout: timeout}
}

// MapPayload is Python _map_payload: an empty field map copies the snapshot; otherwise
// {target: snapshot[source]} for sources whose value is neither None nor "".
func MapPayload(snapshot, fieldMap map[string]any) map[string]any {
	out := make(map[string]any, len(snapshot))
	if len(fieldMap) == 0 {
		for k, v := range snapshot {
			out[k] = v
		}
		return out
	}
	for source, target := range fieldMap {
		v, ok := snapshot[source]
		if !ok || v == nil {
			continue
		}
		if s, isStr := v.(string); isStr && s == "" {
			continue
		}
		out[pyKey(target)] = v
	}
	return out
}

func pyKey(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return colgraph.PyStr(v)
}

// Fingerprint is Python _fingerprint: SHA-256 of json.dumps(payload, sort_keys=True,
// separators=(",", ":"), default=str) with ensure_ascii. Values json cannot encode are
// rendered with str() (default=str); integers of any size are exact.
func Fingerprint(payload map[string]any) string {
	big := map[string]string{}
	b, err := canonjson.Marshal(normalize(payload, big), true)
	if err != nil { // unreachable: normalize only emits encodable values
		b = []byte(fmt.Sprint(payload))
	}
	for ph, digits := range big { // Python ints are arbitrary precision
		b = bytes.ReplaceAll(b, []byte(`"`+ph+`"`), []byte(digits))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// normalize maps a value to what canonjson encodes, applying json.dumps(default=str). Integers
// beyond int64 become unique placeholder strings recorded in big (placeholder -> digits) for
// Fingerprint to splice back in as bare numbers.
func normalize(v any, big map[string]string) any {
	switch x := v.(type) {
	case nil, bool:
		return x
	case string:
		return strings.ToValidUTF8(x, "\uFFFD")
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return uintOf(uint64(x), big)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return uintOf(x, big)
	case float32:
		return floatOf(float64(x))
	case float64:
		return floatOf(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if !strings.ContainsAny(string(x), ".eE") {
			ph := fmt.Sprintf("@@fp-bigint-%d-7c1e@@", len(big))
			if big == nil {
				return string(x)
			}
			big[ph] = string(x)
			return ph
		}
		if f, err := x.Float64(); err == nil {
			return floatOf(f)
		}
		return string(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[strings.ToValidUTF8(k, "\uFFFD")] = normalize(e, big)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[strings.ToValidUTF8(k, "\uFFFD")] = normalize(e, big)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e, big)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e, big)
		}
		return out
	case time.Time:
		return pyDatetime(x)
	}
	return strings.ToValidUTF8(fmt.Sprint(v), "\uFFFD") // default=str
}

func uintOf(u uint64, big map[string]string) any {
	if u > math.MaxInt64 {
		return normalize(json.Number(strconv.FormatUint(u, 10)), big)
	}
	return int64(u)
}

func floatOf(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return f
}

// pyDatetime is str(datetime) for an aware value.
func pyDatetime(t time.Time) string {
	layout := "2006-01-02 15:04:05"
	if t.Nanosecond() != 0 {
		layout += ".000000"
	}
	return t.Format(layout + "-07:00")
}

// IdempotencyKey is dest:<id>:lead:<lead>:<fingerprint>, with ":remove:" before the
// fingerprint for removals.
func IdempotencyKey(destID string, leadID int, fingerprint string, remove bool) string {
	if remove {
		return fmt.Sprintf("dest:%s:lead:%d:remove:%s", destID, leadID, fingerprint)
	}
	return fmt.Sprintf("dest:%s:lead:%d:%s", destID, leadID, fingerprint)
}

// Deliver delivers one snapshot (Python engine._deliver). It never returns an error: a port
// error, a recovered panic and a timeout all become {Success:false, Error}, with known secret
// values redacted and the text cut to 500 characters. Each call is bounded by the Deliverer's
// timeout; a port that ignores its context is abandoned (its goroutine finishes on its own).
func (d *Deliverer) Deliver(ctx context.Context, dest Destination, leadID int, snapshot map[string]any, idem string) Result {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	ch := make(chan Result, 1)
	go func() {
		sec := &secretSet{}
		defer func() {
			if r := recover(); r != nil {
				ch <- failed(fmt.Sprint(r), sec)
			}
		}()
		ch <- d.deliver(ctx, dest, leadID, snapshot, idem, sec)
	}()
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return failed(fmt.Sprintf("delivery timed out after %s", d.timeout), nil)
		}
		return failed("delivery cancelled", nil)
	}
}

func (d *Deliverer) deliver(ctx context.Context, dest Destination, leadID int, snapshot map[string]any, idem string, sec *secretSet) Result {
	mapped := MapPayload(snapshot, dest.FieldMap)
	switch dest.Type {
	case "webhook":
		if d.ports.Webhook == nil {
			return notWired(dest.Type)
		}
		return d.webhook(ctx, dest, leadID, snapshot, mapped, idem, sec)
	case "hubspot", "salesforce":
		if d.ports.CRM == nil {
			return notWired(dest.Type)
		}
		return d.crm(ctx, dest, leadID, snapshot)
	case "instantly", "smartlead":
		return d.campaign(ctx, dest, leadID, mapped, sec)
	case "google_sheets":
		return d.sheets(ctx, dest, leadID, mapped, sec)
	case "airtable":
		return d.airtable(ctx, dest, leadID, mapped, sec)
	case "slack":
		if d.ports.Webhook == nil {
			return notWired(dest.Type)
		}
		return d.slack(ctx, dest, leadID, snapshot, mapped, idem, sec)
	}
	return Result{Error: "unsupported destination '" + dest.Type + "'"}
}

func notWired(typ string) Result {
	return Result{Error: "delivery port for destination '" + typ + "' is not configured"}
}

// secretSet holds secret values resolved during one delivery so any text derived from a port
// error or result can be scrubbed before it leaves this package.
type secretSet struct{ values []string }

func (s *secretSet) add(v string) {
	if s == nil || v == "" {
		return
	}
	s.values = append(s.values, v)
	if u, err := url.Parse(v); err == nil && u.Path != "" && u.Path != "/" {
		s.values = append(s.values, u.Path, strings.ReplaceAll(u.Path, "/", `\/`))
	}
	s.values = append(s.values, strings.ReplaceAll(v, "/", `\/`), url.QueryEscape(v))
}

func (s *secretSet) scrub(text string) string {
	if s == nil {
		return text
	}
	for _, v := range s.values {
		text = strings.ReplaceAll(text, v, redacted)
	}
	return text
}

// failed builds the failure Result: redact first, then cut to 500 characters, so a cut can
// never leave a partial secret behind.
func failed(msg string, sec *secretSet) Result {
	return Result{Error: truncChars(sec.scrub(strings.ToValidUTF8(msg, "\uFFFD")), maxErrorChars)}
}

func truncChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// finish converts a port outcome into the delivery Result. A transport error is a failure; a
// returned Result has its text scrubbed (a port may echo a response body).
func finish(res Result, err error, sec *secretSet) Result {
	if err != nil {
		return failed(err.Error(), sec)
	}
	res.Summary = sec.scrub(res.Summary)
	res.Error = sec.scrub(res.Error)
	return res
}

// pyTruthy is Python truthiness for decoded JSON values.
func pyTruthy(v any) bool {
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
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// strOr is str(v or default).
func strOr(v any, def string) string {
	if !pyTruthy(v) {
		return def
	}
	return colgraph.PyStr(v)
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func rowKey(destID string, leadID int) string { return fmt.Sprintf("dest:%s:lead:%d", destID, leadID) }
