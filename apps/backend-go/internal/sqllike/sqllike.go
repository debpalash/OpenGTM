// Package sqllike builds LIKE/ILIKE patterns from user-supplied text. In a pattern, % and _ are wildcards and
// the escape character is special, so a search for "50%" or "a_b" must have them escaped or it matches far more
// than the user typed (a lone "%" matches every row, "_" every non-empty value).
//
// Always pass the pattern as a query parameter (never concatenate it into SQL) and name the escape character
// in the query: `col ILIKE $2 ESCAPE '\'`. Queries that cannot take a Go-built pattern (sqlc) use the same
// escaping in SQL, see internal/db/queries/emaildata.sql and leads_list.sql.
package sqllike

import "strings"

// Backslash is PostgreSQL's default LIKE escape character. Spell the clause out anyway: ESCAPE '\'.
const Backslash = '\\'

// Escape returns s with the escape character, % and _ each prefixed by the escape character, so the result
// matches s literally when used in a pattern with ESCAPE esc.
func Escape(s string, esc byte) string {
	e := string(esc)
	return strings.NewReplacer(e, e+e, "%", e+"%", "_", e+"_").Replace(s)
}

// Contains returns the pattern "%" + Escape(s) + "%" for ESCAPE '\'.
func Contains(s string) string {
	return "%" + Escape(s, Backslash) + "%"
}
