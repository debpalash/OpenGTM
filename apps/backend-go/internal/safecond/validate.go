package safecond

import (
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// adversarial is the sample value every referenced field is bound to when a
// condition is validated: a quote, AND, a placeholder and OR in one string.
const adversarial = `" AND {x}=="  OR evil`

// Validate is validate_condition: the structure must parse, then the
// condition is evaluated against an adversarial populated row so injection
// style failures surface at create time. A value-driven comparison result is
// fine; only structural errors are returned, as *ConditionError.
func Validate(condition string) (err error) {
	if len(condition) > maxConditionBytes {
		return condErr("condition too long (more than %d bytes)", maxConditionBytes)
	}
	if strings.TrimFunc(condition, colgraph.PyIsSpace) == "" {
		return nil
	}
	switch colgraph.PyLower(colgraph.PyStrip(condition)) {
	case "true", "always", "1", "yes", "false", "never", "0", "no":
		return nil
	}
	toks, err := tokenize(condition)
	if err != nil {
		return err
	}
	if len(toks) == 0 {
		return condErr("empty condition")
	}
	cells := map[string]any{}
	var cols []Column
	for _, t := range toks {
		if t.kind == tPlaceholder {
			if _, seen := cells[t.value]; !seen {
				cells[t.value] = adversarial
				cols = append(cols, Column{ID: t.value, Name: t.value})
			}
		}
	}
	ev := &evaluator{values: buildValues(jsonbOrder(cells), cells, cols), now: float64(time.Now().UnixNano()) / 1e9}
	defer func() {
		if recover() != nil {
			err = condErr("evaluator error: internal") // fail closed
		}
	}()
	if _, err = ev.evalTokens(toks); err != nil {
		low := strings.ToLower(err.Error())
		if strings.Contains(low, "field") && strings.Contains(low, "unresolved") {
			return nil
		}
		return err
	}
	return nil
}
