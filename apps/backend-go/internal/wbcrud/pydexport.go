package wbcrud

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

// Exported views of the pydantic request helpers for sibling packages that
// validate their own request models (runplan: RunWorkbookRequest). They add
// no behaviour; the messages, types and locs are the ones the row routes use.

// PydError is one pydantic error of the given type at loc.
func PydError(typ, msg string, loc []any, in any) []httpx.FieldError { return fe(typ, msg, loc, in) }

// PydModel is the `body is not an object` check of a BaseModel body.
func PydModel(doc any) (*Obj, []httpx.FieldError) { return bodyModel(doc) }

// PydInt is pydantic's lax int; fits is false for an integer beyond int64
// (valid, but it can never name a stored row).
func PydInt(v any, loc []any) (n int64, fits bool, errs []httpx.FieldError) {
	return lenientInt(v, loc)
}

// PydBool is pydantic's lax bool.
func PydBool(v any, loc []any) (bool, []httpx.FieldError) {
	b, errs := parseBool(v, loc)
	if errs != nil {
		return false, errs
	}
	return b.(bool), nil
}

// FilterFromCriteria reads a workbook's stored filter_criteria as a lead filter
// (nil means no filter).
func FilterFromCriteria(v any) (leadsread.Filter, error) {
	if o, ok := v.(*Obj); ok && o != nil {
		return filterFromObj(o)
	}
	return leadsread.Filter{}, nil
}
