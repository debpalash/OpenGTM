// Package aicol runs the workbook AI columns in Go (plan 15 WP-AICOLS): ai_formula
// (one LLM call over the row), research ("Claygent": a bounded web-research agent, legacy
// ReAct loop or native Anthropic tool use), the playbook research_column step and the
// ai_batch request builder, with the prompt-injection guard. Prompts are byte-equal to the
// legacy Python (workbook/ai_column.py, research_column.py, prompt_guard.py,
// playbooks/rpc_research.py); vectors are recorded by testdata/record.py.
//
// The agent column lives in internal/workbook (it needs the run's planner, provider runner
// and spend ledger); this package holds the LLM- and web-facing parts only.
package aicol

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// LLM is the part of *llm.Client the columns use.
type LLM interface {
	Complete(ctx context.Context, req llm.CompleteRequest) (string, error)
	ExtractJSON(ctx context.Context, prompt, system string, maxTokens int) (map[string]any, error)
	AnthropicProvider(ctx context.Context) (llm.Provider, bool)
	AnthropicTurn(ctx context.Context, r llm.AnthropicRequest) (*llm.AnthropicResponse, error)
	BatchComplete(ctx context.Context, reqs []llm.BatchRequest, o llm.BatchOptions) (map[string]string, error)
}

// Settings reads an operator setting (settings.App: stored value, else environment).
type Settings interface {
	Get(ctx context.Context, key, def string) (string, error)
}

// Hit is one search result.
type Hit struct{ Title, Href, Body string }

// Searcher is the keyless web search (_ddg_search). Failures are an empty result.
type Searcher interface {
	Search(ctx context.Context, query string, maxResults int) []Hit
}

// PageFetcher reads one page (UniversalScraper.scrape) and returns its preview text: the
// markdown-ish body text, at most 2000 characters. The URL was already vetted.
type PageFetcher interface {
	Fetch(ctx context.Context, url string) (string, error)
}

// Deps are the collaborators of one column execution.
type Deps struct {
	LLM      LLM
	Settings Settings
	Search   Searcher
	Pages    PageFetcher
	// URLCheck is the SSRF guard for model-chosen URLs (egress.CheckURLFor, strict).
	URLCheck func(rawURL string) error
	Now      func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// setting is _read_setting: the stored value, else def; an unreadable value is def.
func setting(ctx context.Context, s Settings, key, def string) string {
	if s == nil {
		return def
	}
	v, err := s.Get(ctx, key, def)
	if err != nil {
		return def
	}
	return v
}

func settingInt(ctx context.Context, s Settings, key string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(setting(ctx, s, key, strconv.Itoa(def))))
	if err != nil {
		return def
	}
	return n
}

func settingFloat(ctx context.Context, s Settings, key string, def float64) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(setting(ctx, s, key, strconv.FormatFloat(def, 'g', -1, 64))), 64)
	if err != nil {
		return def
	}
	return f
}

// truncRunes is Python s[:n].
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for p := range s {
		if i == n {
			return s[:p]
		}
		i++
	}
	return s
}

// pyStrip is str.strip() (Unicode whitespace).
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

func pyIsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// pyFields is str.split() with no argument.
func pyFields(s string) []string { return strings.FieldsFunc(s, pyIsSpace) }
