// Package signals is the read side of the workspace signal store (port of
// services/signals/store.py and the analytics route). Go never inserts signals:
// the Python scanner owns add_signal and the on_signal automation emit.
package signals

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// ErrBadCursor is Python's ValueError("invalid signal cursor"); routes map it
// to 422 {"detail": "invalid signal cursor"}.
var ErrBadCursor = errors.New("invalid signal cursor")

// EncodeCursor is base64.urlsafe_b64encode(json.dumps([created_at, id],
// separators=(",", ":"))) without padding. NaN/Infinity render as Python's
// json.dumps does (allow_nan default).
func EncodeCursor(createdAt float64, id string) string {
	var ts string
	switch {
	case math.IsNaN(createdAt):
		ts = "NaN"
	case math.IsInf(createdAt, 1):
		ts = "Infinity"
	case math.IsInf(createdAt, -1):
		ts = "-Infinity"
	default:
		ts, _ = canonjson.FloatRepr(createdAt)
	}
	idJSON, err := canonjson.Marshal(id, false)
	if err != nil { // invalid UTF-8 cannot come out of Postgres text
		idJSON, _ = json.Marshal(strings.ToValidUTF8(id, "�"))
	}
	payload := "[" + ts + "," + string(idJSON) + "]"
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// DecodeCursor mirrors SignalStore._decode_cursor, including Python quirks:
// non-alphabet base64 characters are discarded, JSON accepts the bare
// NaN/Infinity/-Infinity literals, the timestamp goes through float() (numbers,
// numeric strings, booleans) and the id must be a string. Anything else is
// ErrBadCursor.
func DecodeCursor(cursor string) (float64, string, error) {
	raw, ok := pyB64Decode(cursor)
	if !ok || !utf8.Valid(raw) {
		return 0, "", ErrBadCursor
	}
	elems, ok := splitTopArray(raw)
	if !ok || len(elems) != 2 {
		return 0, "", ErrBadCursor
	}
	var id string
	if err := json.Unmarshal(elems[1], &id); err != nil || !bytes.HasPrefix(elems[1], []byte(`"`)) {
		return 0, "", ErrBadCursor
	}
	ts, ok := pyFloat(elems[0])
	if !ok {
		return 0, "", ErrBadCursor
	}
	return ts, id, nil
}

// pyB64Decode: urlsafe_b64decode(cursor + "=" * (-len(cursor) % 4)). The
// non-strict decoder drops characters outside the alphabet; a dangling single
// sextet is "incorrect padding".
func pyB64Decode(s string) ([]byte, bool) {
	var clean []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-':
			c = '+'
		case c == '_':
			c = '/'
		}
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' {
			clean = append(clean, c)
		}
	}
	if len(clean)%4 == 1 {
		return nil, false
	}
	out, err := base64.RawStdEncoding.DecodeString(string(clean))
	if err != nil {
		return nil, false
	}
	return out, true
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// splitTopArray splits a JSON document that must be a top-level array into its
// raw element texts. Element validity is checked by the caller; structure
// (strings, nesting) is tracked here so commas inside them do not split.
func splitTopArray(raw []byte) ([][]byte, bool) {
	b := bytes.TrimFunc(raw, func(r rune) bool { return r < 128 && isJSONSpace(byte(r)) })
	if len(b) < 2 || b[0] != '[' || b[len(b)-1] != ']' {
		return nil, false
	}
	b = b[1 : len(b)-1]
	var elems [][]byte
	depth, start := 0, 0
	inStr := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth < 0 {
				return nil, false
			}
		case ',':
			if depth == 0 {
				elems = append(elems, bytes.TrimSpace(b[start:i]))
				start = i + 1
			}
		}
	}
	if inStr || depth != 0 {
		return nil, false
	}
	last := bytes.TrimSpace(b[start:])
	if len(last) == 0 && len(elems) == 0 {
		return nil, true // "[]"
	}
	elems = append(elems, last)
	for _, e := range elems {
		if len(e) == 0 {
			return nil, false
		}
	}
	return elems, true
}

// pyFloat is float(json_value) for the JSON element text.
func pyFloat(e []byte) (float64, bool) {
	switch string(e) {
	case "NaN":
		return math.NaN(), true
	case "Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	case "true":
		return 1, true
	case "false":
		return 0, true
	}
	switch e[0] {
	case '"':
		var s string
		if json.Unmarshal(e, &s) != nil {
			return 0, false
		}
		return pyFloatString(s)
	case '[', '{':
		// valid JSON but float() raises TypeError; invalid JSON raises too
		return 0, false
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(e))
	dec.UseNumber()
	if dec.Decode(&n) != nil || dec.More() {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			// "1e999" -> inf in Python; a huge integer literal -> OverflowError
			if strings.ContainsAny(string(n), ".eE") {
				return f, true
			}
		}
		return 0, false
	}
	return f, true
}

// pyFloatString approximates float(str): surrounding whitespace, nan/inf
// spellings, digit-group underscores; hex floats are rejected like Python.
func pyFloatString(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "xXpP") {
		return 0, false
	}
	if strings.Contains(s, "_") {
		for i := 0; i < len(s); i++ {
			if s[i] == '_' && (i == 0 || i == len(s)-1 || s[i-1] < '0' || s[i-1] > '9' || s[i+1] < '0' || s[i+1] > '9') {
				return 0, false
			}
		}
		s = strings.ReplaceAll(s, "_", "")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}
