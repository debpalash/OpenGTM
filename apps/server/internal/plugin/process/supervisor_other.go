//go:build !unix

package process

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

// MarkerEnv is the run marker variable (unused on this platform).
const MarkerEnv = "OPENGTM_PLUGIN_RUN"

// Supervisor is a stub: process plugins need a unix host.
type Supervisor struct{}

// NewSupervisor always fails on this platform.
func NewSupervisor(Options) (*Supervisor, error) { return nil, ErrUnsupported }

// Run always fails on this platform.
func (*Supervisor) Run(context.Context, *manifest.Plugin, Request) (*Outcome, error) {
	return nil, ErrUnsupported
}

// Close does nothing.
func (*Supervisor) Close() {}

// Stats returns zeros.
func (*Supervisor) Stats() Stats { return Stats{} }

// SweepResult reports what Sweep cleaned up.
type SweepResult struct{ Supervisors, Killed, Removed int }

// Sweep does nothing on this platform.
func Sweep(string, Logger) (SweepResult, error) { return SweepResult{}, nil }
