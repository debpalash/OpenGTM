package copilot

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autopilot"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// Engine is the in-process copilot engine of GO_COPILOT_NATIVE (plan 15 WP-COPILOT-CORE): it replaces the
// Python worker's /internal/copilot/{llm,preroute,describe,system-prompt} RPCs and runs the tools that have a
// Go implementation. Tools without one still go to /internal/copilot/tool.
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

	// Tool collaborators. A nil one sends its tools to the worker.
	Ambition AmbitionClient
	// AmbitionImportExecutor picks the ambitionbox_import job executor (ambitionimport.ExecutorFor).
	AmbitionImportExecutor func(workspaceID string) string
	// RefreshExecutor picks the executor of the refresh_workbook chain the refresh tools may start; nil means
	// "python", as without the Go executor cohort flags.
	RefreshExecutor func(workspaceID string) string
	// WatchExecutor picks the executor of the first watch_poll job track_account_signals schedules
	// (go_executor.watch_poll_executor); nil means "python".
	WatchExecutor func(workspaceID string) string
	// CollectExecutor picks the executor of the collect job start_collection enqueues
	// (config.CollectExecutor: "go" with GO_EXECUTOR_COLLECT and cohort membership); nil means "python".
	CollectExecutor func(workspaceID string) string
	// CollectionLedgerPG is __collection_ledger_backend == "postgres"; start_collection needs it in Go.
	CollectionLedgerPG bool
	// Progress receives start_collection's job_created event (nil: none).
	Progress ProgressEmitter

	// People runs people research, verification and contact enrichment (internal/leadpeople, LeadPeople).
	// nil sends the people tools and the people pre-route branches to the worker.
	People PeopleTools
	// ResolvePerson is entities.resolve_person; create_people_workbook needs it (nil: that tool goes to the
	// worker).
	ResolvePerson PersonResolver
	// Plans is the autopilot plan store (RedisPlanStore when the API has Redis); nil uses one in-process
	// store, which only works with a single API process.
	Plans autopilot.PlanStore
	// Memory is the autopilot run memory; nil uses an in-process one.
	Memory autopilot.Memory
	// NewDraftID overrides the outreach draft id (tests).
	NewDraftID func() string
}

// ProgressEmitter is the leadgen progress bus (*leadprogress.Bus).
type ProgressEmitter interface {
	EmitOrdered(ctx context.Context, workspaceID, eventType string, data leadprogress.Obj) map[string]any
}
