package colgraph

// Exported Python-semantics helpers for internal/safecond (plan 08). They wrap
// the unexported implementations the workbook condition engine already uses so
// the two evaluators cannot drift on float(), str() and whitespace rules.

// PyParseFloat is Python float(str): ok is false where Python raises
// ValueError. It accepts 1e3, inf, nan, underscores and Unicode digits.
func PyParseFloat(s string) (float64, bool) { return parseFloatStrict(s) }

// PyStr is str(value) for a decoded JSON value (json.Number keeps int vs float).
func PyStr(v any) string { return pyStrValue(v) }

// PyStrRepr is repr(str).
func PyStrRepr(s string) string { return pyStrRepr(s) }

// PyStrip is str.strip() with no arguments.
func PyStrip(s string) string { return pyStrip(s) }

// PyLower is str.lower().
func PyLower(s string) string { return pyLower(s) }

// PyIsSpace is str.isspace() for one rune.
func PyIsSpace(r rune) bool { return pyIsSpace(r) }
