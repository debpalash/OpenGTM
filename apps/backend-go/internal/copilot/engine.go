package copilot

import (
	"context"

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
}
