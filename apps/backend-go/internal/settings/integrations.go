package settings

import (
	"context"
	"errors"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

var errNotImplemented = errors.New("settings: integrations not implemented")

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

// WSFieldView is one per-workspace integration field.
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

// Integrations serves the global and per-workspace integration cards.
type Integrations struct {
	List []Integration
	App  *App
	WS   *secrets.Resolver
}

func (g *Integrations) Find(id string) (Integration, bool) { return Integration{}, false }

func (g *Integrations) GlobalViews(ctx context.Context) ([]IntegrationView[FieldView], error) {
	return nil, errNotImplemented
}

func (g *Integrations) GlobalView(ctx context.Context, it Integration) (IntegrationView[FieldView], error) {
	return IntegrationView[FieldView]{}, errNotImplemented
}

func (g *Integrations) SaveGlobal(ctx context.Context, it Integration, vals []KV) ([]string, error) {
	return nil, errNotImplemented
}

func (g *Integrations) WorkspaceViews(ctx context.Context, ws string) ([]IntegrationView[WSFieldView], error) {
	return nil, errNotImplemented
}

func (g *Integrations) WorkspaceView(ctx context.Context, ws string, it Integration) (IntegrationView[WSFieldView], error) {
	return IntegrationView[WSFieldView]{}, errNotImplemented
}

func (g *Integrations) SaveWorkspace(ctx context.Context, ws string, it Integration, vals []KV) ([]string, error) {
	return nil, errNotImplemented
}
