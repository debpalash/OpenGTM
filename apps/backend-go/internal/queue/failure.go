package queue

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FailureNotice is what a failure hook receives; it mirrors the arguments of
// QueueService failure handlers (job_id, payload, reason, will_retry).
type FailureNotice struct {
	JobID       int64
	JobType     string
	WorkspaceID string
	Payload     []byte
	Reason      string
	WillRetry   bool
}

// FailureHook reconciles domain state after a failed attempt was persisted.
type FailureHook func(ctx context.Context, n FailureNotice) error

// hookTimeout bounds one failure hook call.
var hookTimeout = 30 * time.Second

var (
	hookMu sync.RWMutex
	hooks  = map[string]FailureHook{}
)

// RegisterFailureHook sets the hook for jobType (replacing any previous one).
// A nil hook removes it.
func RegisterFailureHook(jobType string, h FailureHook) {
	hookMu.Lock()
	defer hookMu.Unlock()
	if h == nil {
		delete(hooks, jobType)
		return
	}
	hooks[jobType] = h
}

func failureHook(jobType string) FailureHook {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return hooks[jobType]
}

// FinalizeAndNotify runs FinalizeNotify and, after the queue transition has
// committed, calls the hook registered for the job type, exactly like
// queue_service._process_job: only for a failed outcome that was persisted
// (lease not lost, not cancelled). A hook error or panic is logged and never
// changes the job outcome or the returned error.
func FinalizeAndNotify(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, c *Claim, o Outcome) (bool, error) {
	persisted, willRetry, err := FinalizeNotify(ctx, pool, c, o)
	if err != nil || !persisted || o.Status != "failed" {
		return persisted, err
	}
	h := failureHook(c.Type)
	if h == nil {
		return persisted, nil
	}
	if log == nil {
		log = slog.Default()
	}
	reason := o.Error
	if reason == "" {
		reason = "job attempt failed"
	}
	n := FailureNotice{JobID: c.ID, JobType: c.Type, WorkspaceID: c.WorkspaceID,
		Payload: append([]byte(nil), c.Payload...), Reason: reason, WillRetry: willRetry}
	// The hook gets its own context so a slow or timed-out Finalize cannot starve it.
	hctx, hcancel := context.WithTimeout(context.WithoutCancel(ctx), hookTimeout)
	defer hcancel()
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("failure hook panicked", "job_id", c.ID, "job_type", c.Type, "panic", fmt.Sprint(r))
			}
		}()
		if herr := h(hctx, n); herr != nil {
			log.Error("failure hook failed", "job_id", c.ID, "job_type", c.Type, "err", herr)
		}
	}()
	return persisted, nil
}
