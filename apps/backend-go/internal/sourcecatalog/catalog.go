// Package sourcecatalog is the lead-source catalog: routers/settings.py DATA_SOURCES (the user-toggleable
// sources) and source_registry.get_source_summary(), both dumped by Task 1 into the embedded sources.json.
package sourcecatalog

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Obj is an insertion-ordered JSON object.
type Obj = wbcrud.Obj

// Source is one DATA_SOURCES entry.
type Source struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Icon           string `json:"icon"`
	Description    string `json:"description"`
	DefaultEnabled bool   `json:"default_enabled"`
	Strategy       string `json:"strategy"`
	Category       string `json:"category"`
}

//go:embed sources.json
var sourcesJSON []byte

var (
	sources     []Source
	registryRaw json.RawMessage
)

func init() {
	var d struct {
		DataSources     []Source        `json:"data_sources"`
		RegistrySummary json.RawMessage `json:"registry_summary"`
	}
	if err := json.Unmarshal(sourcesJSON, &d); err != nil {
		panic(fmt.Sprintf("sourcecatalog: embedded sources.json: %v", err))
	}
	if _, err := wbcrud.ParseObject(d.RegistrySummary); err != nil {
		panic(fmt.Sprintf("sourcecatalog: embedded registry_summary: %v", err))
	}
	sources, registryRaw = d.DataSources, d.RegistrySummary
}

// Sources returns DATA_SOURCES in Python order. The slice is a copy.
func Sources() []Source { return append([]Source(nil), sources...) }

func find(id string) (Source, bool) {
	for _, s := range sources {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

// Registry is get_source_summary(): total, by_region, by_category and the priority-ordered source list, with the
// key order Python produced. A fresh object is built per call so callers may mutate it.
func Registry() *Obj {
	o, err := wbcrud.ParseObject(registryRaw)
	if err != nil { // validated in init
		return wbcrud.NewObj()
	}
	return o
}
