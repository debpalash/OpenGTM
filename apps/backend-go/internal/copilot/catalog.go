// Package copilot ports the copilot agent's tool catalog, native lead tools, trusted tool context and
// deterministic chat renderers from legacy/python apps/api/services/copilot.
package copilot

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

//go:embed catalog.json
var catalogJSON []byte

// Tool is one copilot tool. Schema is the OpenAI function schema exactly as Python's json.dumps(tool)
// writes it (default separators, ensure_ascii, key order preserved). Level, Label and Reason come from
// Python's DANGEROUS_TOOLS and are empty for tools that are not in it.
type Tool struct {
	Name, Executor       string
	Dangerous            bool
	Level, Label, Reason string
	Schema               json.RawMessage
}

type catalogFile struct {
	Tools []struct {
		Name      string          `json:"name"`
		Executor  string          `json:"executor"`
		Dangerous bool            `json:"dangerous"`
		Schema    json.RawMessage `json:"schema"`
	} `json:"tools"`
	DangerousTools map[string]struct {
		Level  string `json:"level"`
		Label  string `json:"label"`
		Reason string `json:"reason"`
	} `json:"dangerous_tools"`
	SafeTools []string `json:"safe_tools"`
}

type catalogState struct {
	tools []Tool
	safe  map[string]bool
	meta  map[string]Tool // dangerous_tools entries keyed by name (Level/Label/Reason only)
}

var (
	catOnce sync.Once
	cat     catalogState
	catErr  error
)

func loadCatalog() *catalogState {
	catOnce.Do(func() {
		var f catalogFile
		if err := json.Unmarshal(catalogJSON, &f); err != nil {
			catErr = fmt.Errorf("copilot: catalog.json: %w", err)
			return
		}
		cat.safe = make(map[string]bool, len(f.SafeTools))
		for _, n := range f.SafeTools {
			cat.safe[n] = true
		}
		cat.meta = make(map[string]Tool, len(f.DangerousTools))
		for n, m := range f.DangerousTools {
			cat.meta[n] = Tool{Name: n, Level: m.Level, Label: m.Label, Reason: m.Reason}
		}
		for _, t := range f.Tools {
			schema, err := ssestream.Reformat(t.Schema)
			if err != nil {
				catErr = fmt.Errorf("copilot: catalog.json schema %s: %w", t.Name, err)
				return
			}
			m := cat.meta[t.Name]
			cat.tools = append(cat.tools, Tool{
				Name: t.Name, Executor: t.Executor, Dangerous: t.Dangerous,
				Level: m.Level, Label: m.Label, Reason: m.Reason, Schema: schema,
			})
		}
	})
	if catErr != nil {
		panic(catErr) // the catalog is embedded at build time; a parse failure is a build defect
	}
	return &cat
}

// Catalog returns every tool in Python's build_tools() order. The slice is a copy.
func Catalog() []Tool {
	c := loadCatalog()
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// Schemas returns the tool schemas, byte-identical to json.dumps of Python's build_tools() entries.
func Schemas() []json.RawMessage {
	c := loadCatalog()
	out := make([]json.RawMessage, len(c.tools))
	for i, t := range c.tools {
		out[i] = append(json.RawMessage(nil), t.Schema...)
	}
	return out
}

// NeedsConfirmation is Python's needs_confirmation: only explicitly read-only tools run inline, so an
// unknown or hallucinated name fails closed.
func NeedsConfirmation(name string) bool { return !loadCatalog().safe[name] }

// IsDangerous reports whether name is in Python's DANGEROUS_TOOLS (the set that needs tables.write at
// execution time).
func IsDangerous(name string) bool {
	c := loadCatalog()
	if _, ok := c.meta[name]; ok {
		return true
	}
	for _, t := range c.tools {
		if t.Name == name {
			return t.Dangerous
		}
	}
	return false
}

// Meta is the confirmation level and label of a tool, with Python's defaults for unknown names.
func Meta(name string) (level, label string) {
	m, ok := loadCatalog().meta[name]
	if !ok {
		return "high", "Unrecognized write tool"
	}
	level, label = m.Level, m.Label
	if level == "" {
		level = "high"
	}
	if label == "" {
		label = "Unrecognized write tool"
	}
	return level, label
}
