package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

const (
	// DefaultOpenRouterModelsURL is the live model catalogue.
	DefaultOpenRouterModelsURL = "https://openrouter.ai/api/v1/models"
	openRouterTimeout          = 10 * time.Second
	maxOpenRouterBody          = 16 << 20
)

// Models serves GET /api/settings/models/{provider_id}.
type Models struct {
	Reg           *Registry
	HTTP          *http.Client // egress-guarded client
	OpenRouterURL string       // empty = DefaultOpenRouterModelsURL
}

// List returns the response body for a provider; found is false for an unknown
// provider. Static lists come from the registry export; providers without one
// report their default model; openrouter fetches the live catalogue.
func (m *Models) List(ctx context.Context, id string, freeOnly bool) (body any, found bool) {
	prov, ok := m.Reg.Provider(id)
	if !ok {
		return nil, false
	}
	if id == "openrouter" {
		return m.openRouter(ctx, freeOnly), true
	}
	if list, ok := m.Reg.Models.Get(id); ok {
		return map[string]any{"models": list}, true
	}
	return map[string]any{"models": []ModelInfo{{ID: prov.DefaultModel, Name: prov.DefaultModel, Context: 0, Free: true}}}, true
}

type openRouterModel struct {
	ID          string         `json:"id"`
	Name        any            `json:"name"`
	Context     any            `json:"context"`
	Free        bool           `json:"free"`
	Description string         `json:"description"`
	Pricing     map[string]any `json:"pricing"`
}

func (m *Models) openRouter(ctx context.Context, freeOnly bool) any {
	fail := func(err error) any {
		return map[string]any{"models": []openRouterModel{}, "error": scrubErr(err)}
	}
	target := m.OpenRouterURL
	if target == "" {
		target = DefaultOpenRouterModelsURL
	}
	ctx, cancel := context.WithTimeout(ctx, openRouterTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fail(err)
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOpenRouterBody))
	if err != nil {
		return fail(err)
	}
	var data struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return fail(errors.New("invalid JSON response from OpenRouter"))
	}

	models := make([]openRouterModel, 0, len(data.Data))
	for _, e := range data.Data {
		id, ok := e["id"].(string)
		if !ok {
			return fail(errors.New("'id'")) // Python: KeyError('id')
		}
		pricing, _ := e["pricing"].(map[string]any)
		isFree := pricing["prompt"] == "0" && pricing["completion"] == "0"
		if freeOnly && !isFree {
			continue
		}
		var name any = id
		if v, present := e["name"]; present {
			name = v
		}
		var ctxLen any = 0
		if v, present := e["context_length"]; present {
			ctxLen = v
		}
		desc, _ := e["description"].(string)
		var prompt, completion any = "0", "0"
		if v, present := pricing["prompt"]; present {
			prompt = v
		}
		if v, present := pricing["completion"]; present {
			completion = v
		}
		models = append(models, openRouterModel{
			ID: id, Name: name, Context: ctxLen, Free: isFree, Description: truncRunes(desc, 120),
			Pricing: map[string]any{"prompt": prompt, "completion": completion},
		})
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Free != models[j].Free {
			return models[i].Free
		}
		return fmt.Sprint(models[i].Name) < fmt.Sprint(models[j].Name)
	})
	return map[string]any{"models": models, "total": len(models)}
}

// scrubErr is the error text for the response: a transport error without the URL.
func scrubErr(err error) string {
	return unwrapURLErr(err).Error()
}
