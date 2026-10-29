package colcompute

import (
	"bytes"
	"encoding/json"
)

// StoredValue is how enrichment._set_enrichment persists a computed cell value:
// text is the workbook_enrichments.value column (a str as is, anything else
// json.dumps(value, ensure_ascii=False)); raw is the value the row overlay and the
// WebSocket event carry (the Python value itself, as JSON-decodable Go: numbers are
// json.Number so large ints and float reprs survive a re-marshal verbatim).
func StoredValue(v any) (text string, raw any, err error) {
	v = norm(v)
	if s, ok := v.(string); ok {
		return s, s, nil
	}
	text, err = jsonDumps(v, false, false)
	if err != nil {
		return "", nil, err
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		// NaN / Infinity are valid Python json but not JSON: keep the text only.
		return text, text, nil
	}
	return text, raw, nil
}
