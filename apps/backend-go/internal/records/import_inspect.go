package records

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type ImportSample struct {
	Row       int    `json:"row"`
	Value     string `json:"value"`
	Truncated bool   `json:"truncated"`
}

type ImportColumn struct {
	Name               string         `json:"name"`
	Target             string         `json:"target"`
	Match              string         `json:"match"`
	PrecisionSensitive bool           `json:"precision_sensitive"`
	Samples            []ImportSample `json:"samples"`
}

type ImportInspection struct {
	Kind    Kind           `json:"record_type"`
	Rows    int            `json:"rows"`
	Columns []ImportColumn `json:"columns"`
}

// InspectImport is a stateless inspection of the caller's file. It performs no
// database access and tolerates duplicate/invalid mapping choices so they can be
// corrected before the transactional row preview. Suggestions share the exact
// header rules used by Import; samples are strings to preserve numeric IDs.
func InspectImport(request ImportRequest) (ImportInspection, error) {
	rows, headers, err := request.sourceRows()
	if err != nil {
		return ImportInspection{}, err
	}
	out := ImportInspection{Kind: request.Kind, Rows: len(rows), Columns: make([]ImportColumn, 0, len(headers))}
	indices := make(map[string]int, len(headers))
	for _, header := range headers {
		target, matched := suggestedField(header, request.Kind)
		match := "custom"
		if matched {
			match = "matched"
		}
		if strings.HasPrefix(header, "company.") || strings.HasPrefix(header, "person.") || strings.HasPrefix(header, "employment.") {
			match = "explicit"
		}
		if chosen, ok := request.Mapping[header]; ok {
			target, match = chosen, "chosen"
		}
		indices[header] = len(out.Columns)
		out.Columns = append(out.Columns, ImportColumn{Name: header, Target: target, Match: match, Samples: []ImportSample{}})
	}
	for rowIndex, row := range rows {
		for header, value := range row {
			column := &out.Columns[indices[header]]
			if !column.PrecisionSensitive {
				column.PrecisionSensitive = precisionSensitive(value)
			}
			if len(column.Samples) < 3 {
				text := ""
				if scalar, ok := value.(string); ok {
					text = scalar
				} else {
					raw, _ := json.Marshal(value)
					text = string(raw)
				}
				truncated := utf8.RuneCountInString(text) > 160
				if truncated {
					text = string([]rune(text)[:160])
				}
				column.Samples = append(column.Samples, ImportSample{Row: rowIndex + 1, Value: text, Truncated: truncated})
			}
		}
	}
	return out, nil
}

func precisionSensitive(value any) bool {
	switch v := value.(type) {
	case json.Number:
		n, err := strconv.ParseFloat(v.String(), 64)
		return err != nil || math.Abs(n) > 9007199254740991
	case string:
		text := strings.TrimSpace(v)
		if len(text) < 16 {
			return false
		}
		for i, r := range text {
			if i == 0 && (r == '-' || r == '+') {
				continue
			}
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	case map[string]any:
		for _, item := range v {
			if precisionSensitive(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if precisionSensitive(item) {
				return true
			}
		}
	}
	return false
}
