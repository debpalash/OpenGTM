package firmo

import "strings"

// phrase wraps s in double quotes for an exact-phrase search. A double quote
// inside s would end the phrase early and turn the rest into loose search
// operators, so it is dropped.
func phrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, "") + `"`
}
