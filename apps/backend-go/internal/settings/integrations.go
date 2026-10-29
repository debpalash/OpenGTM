package settings

import (
	"context"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// FieldView is one global integration field (secrets are never echoed).
type FieldView struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Secret      bool   `json:"secret"`
	Placeholder string `json:"placeholder"`
	Set         bool   `json:"set"`
	Value       string `json:"value"`
	Masked      string `json:"masked"`
}

// WSFieldView is one per-workspace integration field. Value is always empty.
type WSFieldView struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Secret      bool   `json:"secret"`
	Placeholder string `json:"placeholder"`
	Set         bool   `json:"set"`
	Source      string `json:"source"`
	Value       string `json:"value"`
	Masked      string `json:"masked"`
}

// IntegrationView is the serialized card of one integration.
type IntegrationView[F any] struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Connected   bool   `json:"connected"`
	Fields      []F    `json:"fields"`
}

// KV is one submitted field, in request order.
type KV struct{ Key, Value string }

// Integrations serves the global and per-workspace integration cards
// (routers/settings.py _integration_view, _workspace_integration_view).
type Integrations struct {
	List []Integration
	App  *App              // global store: secret fields are stored encrypted
	WS   *secrets.Resolver // per-workspace store: always encrypted
}

// Find returns the integration with the given id.
func (g *Integrations) Find(id string) (Integration, bool) {
	for _, it := range g.List {
		if it.ID == id {
			return it, true
		}
	}
	return Integration{}, false
}

// lastFour is the secret mask: "…" + last 4 chars, "set" for shorter values.
func lastFour(v string) string {
	r := []rune(v)
	if len(r) >= 4 {
		return "…" + string(r[len(r)-4:])
	}
	return "set"
}

// GlobalViews lists every integration with its global connection state.
func (g *Integrations) GlobalViews(ctx context.Context) ([]IntegrationView[FieldView], error) {
	out := make([]IntegrationView[FieldView], 0, len(g.List))
	for _, it := range g.List {
		v, err := g.GlobalView(ctx, it)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// GlobalView serializes one integration; secrets are never echoed.
func (g *Integrations) GlobalView(ctx context.Context, it Integration) (IntegrationView[FieldView], error) {
	v := IntegrationView[FieldView]{ID: it.ID, Name: it.Name, Icon: it.Icon, Description: it.Description,
		Connected: true, Fields: make([]FieldView, 0, len(it.Fields))}
	for _, f := range it.Fields {
		val, err := g.App.Get(ctx, f.Key, "")
		if err != nil {
			return v, err
		}
		set := val != ""
		v.Connected = v.Connected && set
		fv := FieldView{Key: f.Key, Label: f.Label, Secret: f.Secret, Placeholder: f.Placeholder, Set: set}
		switch {
		case f.Secret && set:
			fv.Masked = lastFour(val)
		case set:
			fv.Masked = "set"
		}
		if !f.Secret {
			fv.Value = val
		}
		v.Fields = append(v.Fields, fv)
	}
	return v, nil
}

// allowedValues keeps only submitted keys that are fields of it, with
// non-blank trimmed values, in request order.
func allowedValues(it Integration, vals []KV) []KV {
	allowed := make(map[string]bool, len(it.Fields))
	for _, f := range it.Fields {
		allowed[f.Key] = true
	}
	var out []KV
	for _, kv := range vals {
		if v := strings.TrimSpace(kv.Value); allowed[kv.Key] && v != "" {
			out = append(out, KV{kv.Key, v})
		}
	}
	return out
}

// SaveGlobal writes the allowed, non-blank fields through App.Set (secret
// keys are stored as ciphertext) and returns the saved keys.
func (g *Integrations) SaveGlobal(ctx context.Context, it Integration, vals []KV) ([]string, error) {
	saved := []string{}
	for _, kv := range allowedValues(it, vals) {
		if err := g.App.Set(ctx, kv.Key, kv.Value); err != nil {
			return saved, err
		}
		saved = append(saved, kv.Key)
	}
	return saved, nil
}

// WorkspaceViews lists every integration for workspace ws.
func (g *Integrations) WorkspaceViews(ctx context.Context, ws string) ([]IntegrationView[WSFieldView], error) {
	out := make([]IntegrationView[WSFieldView], 0, len(g.List))
	for _, it := range g.List {
		v, err := g.WorkspaceView(ctx, ws, it)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// WorkspaceView serializes one integration for ws; no value is ever echoed.
func (g *Integrations) WorkspaceView(ctx context.Context, ws string, it Integration) (IntegrationView[WSFieldView], error) {
	v := IntegrationView[WSFieldView]{ID: it.ID, Name: it.Name, Icon: it.Icon, Description: it.Description,
		Connected: true, Fields: make([]WSFieldView, 0, len(it.Fields))}
	for _, f := range it.Fields {
		wsSet := false
		if !secrets.GlobalOnly[f.Key] {
			var err error
			if wsSet, err = g.WS.Has(ctx, ws, f.Key); err != nil {
				return v, err
			}
		}
		gv, err := g.App.Get(ctx, f.Key, "")
		if err != nil {
			return v, err
		}
		globalSet := gv != ""
		source := "unset"
		switch {
		case wsSet:
			source = "workspace"
		case globalSet:
			source = "global"
		}
		set := wsSet || globalSet
		v.Connected = v.Connected && set
		fv := WSFieldView{Key: f.Key, Label: f.Label, Secret: f.Secret, Placeholder: f.Placeholder, Set: set, Source: source}
		if set {
			fv.Masked = "set"
		}
		v.Fields = append(v.Fields, fv)
	}
	return v, nil
}

// SaveWorkspace stores the allowed, non-blank fields encrypted per workspace.
// Keys in secrets.GlobalOnly are never written at workspace level.
func (g *Integrations) SaveWorkspace(ctx context.Context, ws string, it Integration, vals []KV) ([]string, error) {
	saved := []string{}
	for _, kv := range allowedValues(it, vals) {
		if secrets.GlobalOnly[kv.Key] {
			continue
		}
		if err := g.WS.Set(ctx, ws, kv.Key, kv.Value); err != nil {
			return saved, err
		}
		saved = append(saved, kv.Key)
	}
	return saved, nil
}
