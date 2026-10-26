package runplan

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// Stable reasons of go_executor.go_eligibility (logged by the router).
const (
	ReasonOK                   = "ok"
	ReasonFlagOff              = "flag_off"
	ReasonNotInCohort          = "not_in_cohort"
	ReasonProvenanceOn         = "provenance_on"
	ReasonAutomationsOn        = "automations_on"
	ReasonNoColumns            = "no_columns"
	ReasonColumnType           = "column_type"
	ReasonCondition            = "condition"
	ReasonColumnRefs           = "column_refs"
	ReasonStructuredTarget     = "structured_target"
	ReasonEmailVerify          = "email_verify"
	ReasonEmptyChain           = "empty_chain"
	ReasonNonDeclarative       = "non_declarative_provider"
	ReasonMappingNotTargetOnly = "mapping_not_target_only"
	ReasonManifestExtraEnv     = "manifest_extra_env"
	ReasonEligibilityError     = "eligibility_error"
)

// goColumnTypes are the column types the Go executor runs (go_executor.GO_COLUMN_TYPES).
var goColumnTypes = map[string]bool{"enrichment": true, "waterfall": true}

// extraColumnTypes are the types GO_EXECUTOR_COLUMN_TYPES may add.
var extraColumnTypes = map[string]bool{"formula": true, "http": true, "ai_formula": true, "research": true, "agent": true}

var refRE = regexp.MustCompile(`\{([^}]+)\}`)

// refFields are the config strings scanned for {column} references.
var refFields = []string{"prompt", "formula", "http_url", "goal"}

var errUnhashable = errors.New("runplan: unhashable provider name")

// truthy is bool(v) for the decoded column config values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case string:
		return x != ""
	case bool:
		return x
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return manifest.PyTruthy(v)
}

// firstTruthy is `a or b or c` over config keys: the first truthy value, else
// the last key's value; non-strings are rendered like str().
func firstTruthy(c colgraph.Col, keys ...string) string {
	for _, k := range keys {
		if v := c.Get(k); truthy(v) {
			return manifest.PyStr(v)
		}
	}
	return ""
}

func target(c colgraph.Col) string { return firstTruthy(c, "target_field", "lead_field", "id") }

// hasColumnRefs is go_executor._has_column_refs.
func hasColumnRefs(c colgraph.Col) bool {
	for _, f := range refFields {
		if s, ok := c.Get(f).(string); ok && refRE.MatchString(s) {
			return true
		}
	}
	switch in := c.Get("input_columns").(type) {
	case []any:
		for _, e := range in {
			if s, ok := e.(string); ok && manifest.PyStrip(s) != "" {
				return true
			}
		}
	case string: // `for r in "abc"` iterates characters
		for _, r := range in {
			if manifest.PyStrip(string(r)) != "" {
				return true
			}
		}
	case map[string]any: // iterating a dict yields its keys
		for k := range in {
			if manifest.PyStrip(k) != "" {
				return true
			}
		}
	}
	return false
}

// notAString names a provider entry that is not a string: it can match no manifest.
const notAString = "\x00non-string provider"

func chainName(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case []any, map[string]any:
		return "", errUnhashable
	}
	return notAString, nil
}

// providerChain is go_executor._provider_chain: a configured waterfall (even
// empty), else the single provider, else the target's default waterfall.
func providerChain(c colgraph.Col) ([]string, error) {
	if wf, ok := c.Lookup("waterfall"); ok && wf != nil {
		switch x := wf.(type) {
		case []any:
			out := make([]string, 0, len(x))
			for _, e := range x {
				n, err := chainName(e)
				if err != nil {
					return nil, err
				}
				out = append(out, n)
			}
			return out, nil
		case string: // list("abc")
			out := make([]string, 0, len(x))
			for _, r := range x {
				out = append(out, string(r))
			}
			return out, nil
		case map[string]any: // list(dict) is its keys
			out := make([]string, 0, len(x))
			for k := range x {
				out = append(out, k)
			}
			sort.Strings(out)
			return out, nil
		}
		return nil, errors.New("runplan: waterfall is not iterable")
	}
	if p := c.Get("provider"); truthy(p) {
		n, err := chainName(p)
		if err != nil {
			return nil, err
		}
		return []string{n}, nil
	}
	return workbook.DefaultWaterfall(target(c)), nil
}

// Eligible is go_executor.go_eligibility over the selected columns (every
// configured column whose id the run names, of any type): whether the run may
// execute on Go and, if not, the stable reason Python logs. idx is the manifest
// index (manifest.LoadIndex).
func Eligible(cols []colgraph.Col, idx map[string]manifest.Entry, cfg Config) (bool, string) {
	switch {
	case !cfg.GoExecutorEnabled:
		return false, ReasonFlagOff
	case !cfg.GoCohort[cfg.WorkspaceID]:
		return false, ReasonNotInCohort
	case cfg.ProvenanceTracking:
		return false, ReasonProvenanceOn
	case cfg.AutomationsEnabled:
		return false, ReasonAutomationsOn
	case len(cols) == 0:
		return false, ReasonNoColumns
	}
	for _, c := range cols {
		ct := colType(c)
		extra := cfg.ExtraColumnTypes[ct] && extraColumnTypes[ct]
		if !goColumnTypes[ct] && !extra {
			return false, ReasonColumnType
		}
		if truthy(c.Get("condition")) {
			return false, ReasonCondition
		}
		if hasColumnRefs(c) && len(cfg.ExtraColumnTypes) == 0 { // the Go executor orders referenced columns (task 20)
			return false, ReasonColumnRefs
		}
		if extra {
			continue // no provider chain, structured target or email verification
		}
		t := target(c)
		if workbook.IsStructuredField(t) && len(cfg.ExtraColumnTypes) == 0 {
			return false, ReasonStructuredTarget
		}
		if v, present := c.Lookup("verify"); t == "email" && (!present || truthy(v)) {
			return false, ReasonEmailVerify
		}
		chain, err := providerChain(c)
		if err != nil {
			return false, ReasonEligibilityError
		}
		if len(chain) == 0 {
			return false, ReasonEmptyChain
		}
		for _, name := range chain {
			e, ok := idx[name]
			if !ok {
				return false, ReasonNonDeclarative
			}
			m := e.Manifest
			if len(m.Response.Mappings) != 1 {
				return false, ReasonMappingNotTargetOnly
			}
			if _, only := m.Response.Mappings[t]; !only {
				return false, ReasonMappingNotTargetOnly
			}
			if problem := manifest.SecurityProblem(m); problem != "" {
				return false, problem
			}
			for env := range manifest.AllowedEnvNames(m) {
				if env != m.Auth.EnvVar { // the credential endpoint serves only auth.env_var
					return false, ReasonManifestExtraEnv
				}
			}
		}
	}
	return true, ReasonOK
}
