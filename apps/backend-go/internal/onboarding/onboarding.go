// Package onboarding computes the first-run checklist (GET /api/onboarding) from real workspace data. Nothing
// is stored: each step is derived on every call, so a step flips the moment the user does the thing (and flips
// back if they undo it).
package onboarding

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/demoseed"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Step ids, in checklist order. The web client relies on these exact strings.
const (
	StepLLMProvider = "llm_provider"
	StepDataSource  = "data_source"
	StepData        = "data"
	StepEnrichment  = "enrichment"
)

// Step is one checklist item.
type Step struct {
	ID   string `json:"id"`
	Done bool   `json:"done"`
}

// State is the GET /api/onboarding body.
type State struct {
	Steps     []Step `json:"steps"`
	DoneCount int    `json:"done_count"`
	Total     int    `json:"total"`
	Complete  bool   `json:"complete"`
	// SampleWorkbookID is the id of this workspace's sample workbook when it exists, else null.
	SampleWorkbookID *string `json:"sample_workbook_id"`
}

// Service answers the checks that need more than the workspace's own tables. The two functions are seams over
// the settings layer (LLM provider keys, data source toggles); a nil one counts as "not done".
type Service struct {
	Pool *pgxpool.Pool
	// LLMUsable reports whether the workspace can call a language model: a provider key set for the workspace or
	// globally, or a keyless custom endpoint configured.
	LLMUsable func(ctx context.Context, workspaceID string) (bool, error)
	// SourceEnabled reports whether at least one data source is enabled.
	SourceEnabled func(ctx context.Context) (bool, error)
}

// Get computes the state for workspaceID. The workspace tables are read inside a tenancy transaction, so Row
// Level Security applies on top of the explicit workspace filter.
func (s *Service) Get(ctx context.Context, workspaceID string) (State, error) {
	var llm, src bool
	var err error
	if s.LLMUsable != nil {
		if llm, err = s.LLMUsable(ctx, workspaceID); err != nil {
			return State{}, fmt.Errorf("onboarding: llm check: %w", err)
		}
	}
	if s.SourceEnabled != nil {
		if src, err = s.SourceEnabled(ctx); err != nil {
			return State{}, fmt.Errorf("onboarding: source check: %w", err)
		}
	}
	var prog gen.OnboardingProgressRow
	var sample bool
	sampleID := demoseed.SampleWorkbookID(workspaceID)
	err = tenancy.InWorkspace(ctx, s.Pool, workspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		var e error
		if prog, e = q.OnboardingProgress(ctx, workspaceID); e != nil {
			return e
		}
		sample, e = q.OnboardingWorkbookExists(ctx, gen.OnboardingWorkbookExistsParams{WorkspaceID: workspaceID, ID: sampleID})
		return e
	})
	if err != nil {
		return State{}, fmt.Errorf("onboarding: workspace data: %w", err)
	}
	st := State{Steps: []Step{
		{StepLLMProvider, llm}, {StepDataSource, src}, {StepData, prog.HasData}, {StepEnrichment, prog.HasEnrichment},
	}}
	for _, step := range st.Steps {
		if step.Done {
			st.DoneCount++
		}
	}
	st.Total = len(st.Steps)
	st.Complete = st.DoneCount == st.Total
	if sample {
		st.SampleWorkbookID = &sampleID
	}
	return st, nil
}
