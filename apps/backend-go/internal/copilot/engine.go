package copilot

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autopilot"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// Engine is the in-process copilot engine (plan 15 WP-COPILOT-CORE/TOOLS, the port of services/copilot): it runs
// the llm rounds, the pre-routes, the action descriptions and the engine tools. A tool whose collaborator is
// nil answers a tool error.
type Engine struct {
	// LLM is the workspace's LLM client (llm.New with the egress-guarded operator client).
	LLM *llm.Client
	// Chain overrides the chat provider chain (tests); nil uses LLM.ChatChain, or the keyless default.
	Chain func(ctx context.Context) []llm.ChatProvider
	// IntentPollerEnabled and PGLeadStore are settings.INTENT_POLLER_ENABLED / PG_LEAD_STORE (the signal
	// tracking pre-route and track_account_signals refuse without them).
	IntentPollerEnabled, PGLeadStore bool
	// NewID overrides uuid4().hex for approval tool-call ids and collection job ids (tests).
	NewID func() string

	// Tool collaborators. A nil one makes its tools unavailable (a tool error).
	Ambition AmbitionClient
	// AmbitionImportExecutor picks the ambitionbox_import job executor (ambitionimport.ExecutorFor).
	AmbitionImportExecutor func(workspaceID string) string
	// CollectionLedgerPG is __collection_ledger_backend == "postgres"; start_collection needs it in Go.
	CollectionLedgerPG bool
	// Progress receives start_collection's job_created event (nil: none).
	Progress ProgressEmitter

	// People runs people research, verification and contact enrichment (internal/leadpeople, LeadPeople).
	// nil makes the people tools and the people pre-route branches unavailable.
	People PeopleTools
	// ResolvePerson is entities.resolve_person; create_people_workbook needs it (nil: that tool is unavailable).
	ResolvePerson PersonResolver
	// Plans is the autopilot plan store (RedisPlanStore when the API has Redis); nil uses one in-process
	// store, which only works with a single API process.
	Plans autopilot.PlanStore
	// Memory is the autopilot run memory; nil uses an in-process one.
	Memory autopilot.Memory
	// Providers finds the in-process providers enrich_lead runs (website_scraper, ddg_email); nil makes
	// enrich_lead unavailable. URLCheck refuses a lead website the egress policy forbids before a provider
	// dials it (nil: no check).
	Providers ProviderLookup
	URLCheck  func(rawURL string) error
	// NewDraftID overrides the outreach draft id (tests).
	NewDraftID func() string
}

// ProgressEmitter is the leadgen progress bus (*leadprogress.Bus).
type ProgressEmitter interface {
	EmitOrdered(ctx context.Context, workspaceID, eventType string, data leadprogress.Obj) map[string]any
}
