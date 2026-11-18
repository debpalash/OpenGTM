# Optional web providers

The Go backend can use these operator-configured APIs independently:

| Provider | Role | Setup |
| --- | --- | --- |
| Firecrawl | Opt-in fallback web search with plain snippets | [Firecrawl setup](providers/firecrawl.md) |
| Keenable | Opt-in fallback web search (key or explicit keyless flag) | [Keenable setup](providers/keenable.md) |
| Spicrawl | Recommended page/PDF-to-markdown provider for research | [Spicrawl setup](providers/spicrawl.md) |

Each provider remains inactive without its own server-side API key (Keenable also accepts an explicit `KEENABLE_ENABLED=true` for keyless mode). Research can
use Firecrawl search and Spicrawl page reads without the optional browser sidecar.
The browser pipeline remains available when configured. Provider spend is
separate from LLM token budgets; Spicrawl requests enforce explicit credit ceilings.
