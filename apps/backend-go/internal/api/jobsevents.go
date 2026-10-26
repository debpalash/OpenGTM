package api

import (
	"bytes"
	"encoding/json"
	"io"
)

// compactStoredEvent re-renders a stored event the way FastAPI would return json.loads(stored):
// keys in stored order (a decoded Go map loses it), compact separators, non-ASCII raw
// (ensure_ascii=False). Numbers keep their stored text (Python wrote them in repr form already).
// ok is false for text that is not one well-formed JSON value.
func compactStoredEvent(raw string) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var out bytes.Buffer
	if !compactValue(dec, &out) {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return pyJSON(out.Bytes()), true
}

func compactValue(dec *json.Decoder, out *bytes.Buffer) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			out.WriteByte('{')
			for first := true; dec.More(); first = false {
				if !first {
					out.WriteByte(',')
				}
				k, err := dec.Token()
				ks, isStr := k.(string)
				if err != nil || !isStr {
					return false
				}
				writeJSONString(out, ks)
				out.WriteByte(':')
				if !compactValue(dec, out) {
					return false
				}
			}
			_, err = dec.Token() // '}'
			out.WriteByte('}')
			return err == nil
		case '[':
			out.WriteByte('[')
			for first := true; dec.More(); first = false {
				if !first {
					out.WriteByte(',')
				}
				if !compactValue(dec, out) {
					return false
				}
			}
			_, err = dec.Token() // ']'
			out.WriteByte(']')
			return err == nil
		}
		return false
	case string:
		writeJSONString(out, t)
	case json.Number:
		out.WriteString(t.String())
	case bool:
		if t {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case nil:
		out.WriteString("null")
	}
	return true
}

// writeJSONString writes s with encoding/json escapes but without HTML escaping; pyJSON then turns
// U+2028/U+2029 into the raw characters.
func writeJSONString(out *bytes.Buffer, s string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out.Write(bytes.TrimRight(b.Bytes(), "\n"))
}
