package llm

import (
	"context"
)

func (c *Client) resolveChat(ctx context.Context, id string) (ChatProvider, bool) {
	if c.o.Registry == nil {
		return ChatProvider{}, false
	}
	prov, ok := c.o.Registry.Provider(id)
	if !ok {
		return ChatProvider{}, false
	}
	key := c.get(ctx, prov.EnvKey, "")
	base := c.get(ctx, prov.EnvURL, "")
	if base == "" {
		base = prov.DefaultURL
	}
	model := c.get(ctx, prov.EnvModel, "")
	if model == "" {
		model = prov.DefaultModel
	}
	if key == "" || base == "" {
		return ChatProvider{}, false
	}
	name := prov.Name
	if name == "" {
		name = id
	}
	return ChatProvider{ID: id, Name: name, APIKey: key, BaseURL: base, Model: model, OpenAICompatible: prov.OpenAICompatible}, true
}

// ChatChain returns the configured chat providers in priority order: the
// default (LLM_DEFAULT_PROVIDER, "openrouter" when unset) first, then every
// other configured OpenAI-compatible provider in registry order.
// (_get_provider_chain.) The default is included even when it is not
// OpenAI-compatible, as in Python.
func (c *Client) ChatChain(ctx context.Context) []ChatProvider {
	if c.o.Registry == nil {
		return nil
	}
	def := c.get(ctx, "LLM_DEFAULT_PROVIDER", "openrouter")
	var chain []ChatProvider
	seen := map[string]bool{}
	if p, ok := c.resolveChat(ctx, def); ok {
		chain = append(chain, p)
		seen[def] = true
	}
	for _, id := range c.o.Registry.Providers.Keys() {
		if seen[id] {
			continue
		}
		if prov, _ := c.o.Registry.Provider(id); !prov.OpenAICompatible {
			continue
		}
		if p, ok := c.resolveChat(ctx, id); ok {
			chain = append(chain, p)
			seen[id] = true
		}
	}
	return chain
}

// ActiveChatProvider is the head of ChatChain. With nothing configured it
// returns the default provider without a key (the caller's Stream then reports
// "No AI provider configured").
func (c *Client) ActiveChatProvider(ctx context.Context) ChatProvider {
	if chain := c.ChatChain(ctx); len(chain) > 0 {
		return chain[0]
	}
	def := c.get(ctx, "LLM_DEFAULT_PROVIDER", "openrouter")
	if c.o.Registry == nil {
		return ChatProvider{ID: def, Name: def, OpenAICompatible: true}
	}
	prov, ok := c.o.Registry.Provider(def)
	if !ok {
		prov, _ = c.o.Registry.Provider("openrouter")
	}
	name := prov.Name
	if name == "" {
		name = def
	}
	return ChatProvider{ID: def, Name: name, BaseURL: prov.DefaultURL, Model: prov.DefaultModel, OpenAICompatible: prov.OpenAICompatible}
}

// ChatCursor is the starting Cursor of a chat round: the chain head and the rest as fallbacks.
func (c *Client) ChatCursor(ctx context.Context) Cursor {
	chain := c.ChatChain(ctx)
	if len(chain) == 0 {
		return Cursor{Provider: c.ActiveChatProvider(ctx)}
	}
	return Cursor{Provider: chain[0], Fallbacks: chain[1:]}
}
