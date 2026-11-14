package llm

import (
	"context"
	"net/http"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// CustomOpenAIBaseURLKey is the setting holding the self-hosted
// OpenAI-compatible server's base URL. It is exported so the HTTP wiring can
// watch the same key the client reads.
const CustomOpenAIBaseURLKey = "CUSTOM_OPENAI_BASE_URL"

// OperatorClient builds the outbound client for provider traffic: the static
// operator allow-list (EGRESS_ALLOW_HOSTS) plus the host of
// CUSTOM_OPENAI_BASE_URL, re-read from the settings store on every dial so a
// Settings-UI change takes effect without a restart or image rebuild. The
// dynamic entry is vetted by egress.RuntimeProviderHost (no localhost, no
// metadata or link-local IPs); everything else keeps the full SSRF guard.
// settings may be nil (static allow-list only).
func OperatorClient(static []string, settings Settings) *http.Client {
	return egress.NewOperatorClientFunc(func() []string {
		out := append([]string(nil), static...)
		if settings == nil {
			return out
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		base, err := settings.Get(ctx, CustomOpenAIBaseURLKey, "")
		if err != nil || base == "" {
			return out
		}
		if host, ok := egress.RuntimeProviderHost(base); ok {
			out = append(out, host)
		}
		return out
	})
}
