package sourcecatalog

import (
	"context"
	"strings"
	"unicode"
)

// Getter is the global settings read (settings.App.Get): the stored value, else the environment, else def.
type Getter interface {
	Get(ctx context.Context, key, def string) (string, error)
}

// Setter is the global settings write (settings.App.Set).
type Setter interface {
	Set(ctx context.Context, key, value string) error
}

func settingKey(id string) string { return "SOURCE_" + pyUpper(id) + "_ENABLED" }

// Enabled is get_source_enabled: SOURCE_<ID>_ENABLED, where "" means unset (the catalog default; false for an
// unknown id) and anything else is enabled only when exactly "1". The key upper-cases the id even when the id is
// not a catalog id, as Python does. A settings read error falls back to the default (Python's _db_get swallows
// read errors the same way).
func Enabled(ctx context.Context, settings Getter, id string) bool {
	src, _ := find(id)
	def := src.DefaultEnabled
	if settings == nil {
		return def
	}
	v, err := settings.Get(ctx, settingKey(id), "")
	if err != nil || v == "" {
		return def
	}
	return v == "1"
}

// SetEnabled stores "1" or "0" under SOURCE_<ID>_ENABLED (the PUT /settings/sources/{id} write).
func SetEnabled(ctx context.Context, settings Setter, id string, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return settings.Set(ctx, settingKey(id), v)
}

// pyUpper is str.upper for the characters whose Python mapping is longer than Go's simple mapping.
func pyUpper(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case 'ß':
			b.WriteString("SS")
		case 'ŉ':
			b.WriteString("ʼN")
		case 'ǰ':
			b.WriteString("J̌")
		case 'ΐ':
			b.WriteString("Ϊ́")
		case 'ΰ':
			b.WriteString("Ϋ́")
		case 'և':
			b.WriteString("ԵՒ")
		case 'ﬀ':
			b.WriteString("FF")
		case 'ﬁ':
			b.WriteString("FI")
		case 'ﬂ':
			b.WriteString("FL")
		case 'ﬃ':
			b.WriteString("FFI")
		case 'ﬄ':
			b.WriteString("FFL")
		case 'ﬅ', 'ﬆ':
			b.WriteString("ST")
		default:
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}
