package wbcrud

import (
	"encoding/json"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// ViewCreateBody is WorkbookViewCreate; ViewUpdateBody is WorkbookViewUpdate
// (nil Name / Config = field omitted or null, i.e. unchanged).
type ViewCreateBody struct {
	Name   string
	Config ViewConfig
}

type ViewUpdateBody struct {
	Name   *string
	Config *ViewConfig
}

// fastAPIViewErrors maps ParseViewConfig's model_validate errors to the body
// form FastAPI answers (from_attributes: model_attributes_type, and the
// literal_error ctx.expected).
func fastAPIViewErrors(errs []httpx.FieldError) []httpx.FieldError {
	for i := range errs {
		e := &errs[i]
		switch e.Type {
		case "model_type":
			e.Type = "model_attributes_type"
			e.Msg = "Input should be a valid dictionary or object to extract fields from"
		case "string_too_short":
			e.Ctx = map[string]any{"min_length": 1}
		case "literal_error":
			e.Ctx = map[string]any{"expected": strings.TrimPrefix(e.Msg, "Input should be ")}
		}
	}
	return errs
}

// viewConfigField validates the body's "config" value (present and not null
// unless allowNull).
func viewConfigField(raw json.RawMessage) (ViewConfig, []httpx.FieldError) {
	cfg, errs := ParseViewConfig(raw, "body", "config")
	return cfg, fastAPIViewErrors(errs)
}

func parseViewName(v any, errs *[]httpx.FieldError) string {
	s, e := strConstrained(v, bodyLoc("name"), 1, 255)
	*errs = append(*errs, e...)
	return s
}

func rawFields(raw []byte) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// ParseViewCreateBody validates {"name": 1..255 chars, "config": ViewConfig}.
func ParseViewCreateBody(raw []byte) (ViewCreateBody, []httpx.FieldError) {
	o, errs := bodyObject(raw)
	if errs != nil {
		return ViewCreateBody{}, errs
	}
	b := ViewCreateBody{Config: ViewConfig{Filters: []ViewFilter{}, Sort: []ViewSort{}, HiddenColumns: []string{}}}
	if v, ok := o.Get("name"); ok {
		b.Name = parseViewName(v, &errs)
	} else {
		errs = append(errs, missingErr(bodyLoc("name"), o))
	}
	if cr, ok := rawFields(raw)["config"]; ok {
		cfg, cerrs := viewConfigField(cr)
		errs = append(errs, cerrs...)
		b.Config = cfg
	}
	if len(errs) > 0 {
		return ViewCreateBody{}, errs
	}
	return b, nil
}

// ParseViewUpdateBody validates {"name"?: 1..255 chars|null, "config"?: ViewConfig|null}.
func ParseViewUpdateBody(raw []byte) (ViewUpdateBody, []httpx.FieldError) {
	o, errs := bodyObject(raw)
	if errs != nil {
		return ViewUpdateBody{}, errs
	}
	var b ViewUpdateBody
	if v, ok := o.Get("name"); ok && v != nil {
		s := parseViewName(v, &errs)
		b.Name = &s
	}
	if cr, ok := rawFields(raw)["config"]; ok && string(cr) != "null" {
		cfg, cerrs := viewConfigField(cr)
		errs = append(errs, cerrs...)
		b.Config = &cfg
	}
	if len(errs) > 0 {
		return ViewUpdateBody{}, errs
	}
	return b, nil
}
