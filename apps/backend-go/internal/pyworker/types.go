// Package pyworker is the boundary to the Python worker's POST
// /internal/provider-call RPC (decision D8). This file holds the wire types, now aliases of internal/provcall
// (plan 15: the package is retired once no caller imports it).
package pyworker

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"

// ProviderCall is one provider invocation delegated to Python.
type ProviderCall = provcall.Call

// ProviderResult is the Python answer.
type ProviderResult = provcall.Result
