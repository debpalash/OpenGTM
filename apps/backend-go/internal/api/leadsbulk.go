package api

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// enqueueJob is the queue seam (tests swap it to force an enqueue failure).
var enqueueJob = queue.Enqueue

// registerBulk is filled by plan 05; the stub registers nothing.
func registerBulk(r *httpx.Router, d Deps) {}
