package main

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/server/internal/kernels"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/pluginrun"
)

// Declarative scrapers extract with the embedded Rust kernel.
func init() {
	pluginrun.Version = version
	kernelExtractor = func(ctx context.Context) (declarative.Extractor, func(), error) {
		k, err := kernels.Load(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		return k, func() { _ = k.Close() }, nil
	}
}
