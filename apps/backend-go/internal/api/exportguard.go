package api

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// Defaults for the CSV export guards (final-review I2): an export holds a pooled
// connection and a transaction while it streams, so a stalled reader must not pin
// them and the number of concurrent exports is bounded.
const (
	defaultExportMax          = 8
	defaultExportPerWorkspace = 2
	defaultExportWriteTimeout = 15 * time.Second
)

// exportLimiter bounds concurrent CSV exports overall and per workspace.
type exportLimiter struct {
	mu       sync.Mutex
	global   int
	perWS    int
	inFlight int
	byWS     map[string]int
}

func newExportLimiter(global, perWorkspace int) *exportLimiter {
	if global <= 0 {
		global = defaultExportMax
	}
	if perWorkspace <= 0 {
		perWorkspace = defaultExportPerWorkspace
	}
	return &exportLimiter{global: global, perWS: perWorkspace, byWS: map[string]int{}}
}

// tryAcquire takes a slot without waiting; ok is false when a cap is reached.
// release is idempotent.
func (l *exportLimiter) tryAcquire(ws string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight >= l.global || l.byWS[ws] >= l.perWS {
		return nil, false
	}
	l.inFlight++
	l.byWS[ws]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.inFlight--
			if l.byWS[ws]--; l.byWS[ws] <= 0 {
				delete(l.byWS, ws)
			}
		})
	}, true
}

type exportDeadlineWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

// newDeadlineWriter wraps w so each Write first extends the connection write
// deadline by timeout: a client that stops reading ends the export (the write
// fails) instead of pinning the database connection. Writers that cannot set
// a deadline are used as is.
func newDeadlineWriter(w http.ResponseWriter, timeout time.Duration) io.Writer {
	return &exportDeadlineWriter{w: w, rc: http.NewResponseController(w), timeout: timeout}
}

func (d *exportDeadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
	return d.w.Write(p)
}
