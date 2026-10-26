package api

import (
	"io"
	"net/http"
	"time"
)

// exportLimiter bounds concurrent CSV exports (stub).
type exportLimiter struct{}

func newExportLimiter(global, perWorkspace int) *exportLimiter { return &exportLimiter{} }

func (l *exportLimiter) tryAcquire(ws string) (release func(), ok bool) { return func() {}, true }

// newDeadlineWriter wraps w so each Write first extends the write deadline (stub).
func newDeadlineWriter(w http.ResponseWriter, timeout time.Duration) io.Writer { return w }
