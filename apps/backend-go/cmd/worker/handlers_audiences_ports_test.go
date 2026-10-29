package main

import (
	"context"
	"io"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destvalidate"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

type emptyStore struct{}

func (emptyStore) AppSetting(context.Context, string) (string, error) { return "", nil }
func (emptyStore) WorkspaceSetting(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// Every destination type the API accepts has a Go delivery port in the production wiring, with or without the local
// resolver: no sync fails permanently as "no Go delivery port".
func TestDestPortsCoverEveryAcceptedType(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	for name, sec := range map[string]destdeliver.SecretResolver{
		"nil":   nil,
		"local": secrets.NewResolver(nil, emptyStore{}, func(string) string { return "" }),
	} {
		p := destPorts(config.Config{}, nil, log, sec, audienceOpts{})
		for _, typ := range destvalidate.Types {
			if !p.Supports(typ) {
				t.Errorf("%s resolver: %s has no Go delivery port", name, typ)
			}
		}
	}
}
