package api

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp"
)

// TestCapabilityVocabularyEqualsPlan02Tokens: the capability set accepted by
// /api/mcp/tokens (plan 02) equals the MCP server's AllCaps.
func TestCapabilityVocabularyEqualsPlan02Tokens(t *testing.T) {
	if len(allMCPCapabilities) != len(mcp.AllCaps) {
		t.Fatalf("token vocabulary %d, mcp.AllCaps %d", len(allMCPCapabilities), len(mcp.AllCaps))
	}
	for c := range allMCPCapabilities {
		if !mcp.AllCaps[c] {
			t.Errorf("%s accepted by /api/mcp/tokens but unknown to mcp.AllCaps", c)
		}
	}
	for c := range mcp.AllCaps {
		if !allMCPCapabilities[c] {
			t.Errorf("%s in mcp.AllCaps but rejected by /api/mcp/tokens", c)
		}
	}
}
