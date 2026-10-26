// Package dsnparse is the Go port of apps/api/services/outreach/dsn_parse.py:
// RFC 3464 delivery status notifications, RFC 5965 ARF complaints and a
// conservative plain-text bounce heuristic. It is a pure function over bytes
// (no I/O). Behaviour matches the Python reference on the recorded corpus
// (testdata/dsn), quirks included; the only deliberate deviations are the
// bounded-work hardening (D6) and the documented divergences in mime.go /
// pyutil.go (RFC 2231 parameters and uuencoded bodies are not decoded).
package dsnparse

import (
	"errors"
	"regexp"
	"strings"
)

// Record is one classified feedback record extracted from an inbound message.
type Record struct {
	Recipient         string
	Kind              string // hard | soft | complaint | unknown
	Diagnostic        string
	OriginalMessageID string
}

// errPython marks the places where the Python reference raises (a non-ASCII
// header value is an email.header.Header, which has no str methods). Python's
// parse_message lets the exception escape; the Go equivalent is "no records".
var errPython = errors.New("dsnparse: python would raise")

// Parse classifies an inbound message into zero or more records
// (parse_message). Input over 1 MiB is truncated before parsing and MIME
// nesting deeper than 8 levels is ignored. Any failure yields nil.
func Parse(raw []byte) (recs []Record) {
	defer func() {
		if recover() != nil {
			recs = nil
		}
	}()
	raw = bound(raw)
	root := parseMessage(raw)
	ctype := root.contentType()
	reportType := ""
	if v, ok := root.param("report-type"); ok {
		reportType = strings.ToLower(v)
	}
	if ctype == "multipart/report" && reportType == "delivery-status" {
		r, err := parseDSN(root)
		if err != nil {
			return nil
		}
		if len(r) > 0 {
			return r
		}
	}
	if ctype == "multipart/report" && reportType == "feedback-report" {
		r, err := parseARF(root)
		if err != nil {
			return nil
		}
		if len(r) > 0 {
			return r
		}
	}
	r, err := parseHeuristic(root)
	if err != nil {
		return nil
	}
	return r
}

// SourceMessageID is the inbound message's own Message-ID (source_message_id).
func SourceMessageID(raw []byte) (id string) {
	defer func() {
		if recover() != nil {
			id = ""
		}
	}()
	v, err := headerStr(rootHeaders(bound(raw)), "message-id")
	if err != nil {
		return ""
	}
	return pyStrip(v)
}

func bound(raw []byte) []byte {
	if len(raw) > maxInput {
		return raw[:maxInput]
	}
	return raw
}

// headerStr is `(msg.get(name) or "")` followed by a str method: a value with
// bytes >= 0x80 is a Header object and the Python call raises.
func headerStr(p *part, name string) (string, error) {
	v, ok := p.getRaw(name)
	if !ok || v == "" {
		return "", nil
	}
	if hasHigh(v) {
		return "", errPython
	}
	return v, nil
}

// originalMessageID is _extract_original_message_id.
func originalMessageID(orig *part) (string, error) {
	if orig == nil {
		return "", nil
	}
	switch orig.contentType() {
	case "message/rfc822":
		if sub := firstChild(orig); sub != nil {
			v, err := headerStr(sub, "message-id")
			return pyStrip(v), err
		}
	case "text/rfc822-headers":
		if raw := payloadBytes(orig); len(raw) > 0 {
			v, err := headerStr(rootHeaders(raw), "message-id")
			return pyStrip(v), err
		}
	}
	return "", nil
}

func firstChild(p *part) *part {
	if p.isList && len(p.children) > 0 {
		return p.children[0]
	}
	return nil
}

// statusBlocks is _status_blocks. The text fallback only matters for parts the
// depth bound turned into empty leaves, which Python never produces for these
// content types, so such parts yield nothing.
func statusBlocks(p *part) []*part {
	if p.isList {
		return p.children
	}
	return nil
}

var emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// addr is _addr: parse a *-Recipient value ("rfc822; user@example.com").
func addr(value string) string {
	if value == "" {
		return ""
	}
	tail := value
	if i := strings.IndexByte(value, ';'); i >= 0 {
		tail = value[i+1:]
	}
	tail = pyStrip(tail)
	if m := emailRE.FindString(tail); m != "" {
		tail = strings.ToLower(m)
	}
	return strings.Trim(tail, "<> ")
}
