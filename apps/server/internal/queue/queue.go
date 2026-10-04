// Package queue is the Go executor for the durable PostgreSQL jobs queue.
//
// It shares the jobs table with the Python QueueService
// (apps/api/services/queue_service.py) and reproduces its semantics: claim
// eligibility and ordering, the per-workspace active cap, heartbeats, the
// (worker_id, locked_at) lease, guarded finalization that preserves
// cancellation, retry backoff and the error strings operators already grep
// for. The only intentional difference is ownership: Go claims a type only
// when job_executor_routes routes it to 'go', and Python claims only types
// not routed elsewhere, so each job has exactly one executor.
//
// jobs timestamps are "timestamp without time zone". Every timestamp written
// here is computed by PostgreSQL (LOCALTIMESTAMP, i.e. now() in the session
// time zone), which is the same conversion psycopg applies to the aware UTC
// datetimes Python binds, and avoids depending on worker clocks.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
)

// Lease identifies one claim attempt. A worker may only heartbeat or finalize
// a job while both fields still match the row, so a stale attempt can never
// overwrite a reclaimed one.
type Lease struct {
	WorkerID string
	LockedAt time.Time
}

// Job is a claimed job handed to a Handler.
type Job struct {
	ID          int64
	Type        string
	Payload     json.RawMessage
	WorkspaceID string
	RetryCount  int
	Lease       Lease
}

// Handler runs one attempt. It must honour ctx: ctx is cancelled on timeout,
// when the claim is cancelled or reclaimed, and at the end of the shutdown
// grace period. Domain writes should be fenced by Job.Lease.
type Handler func(ctx context.Context, job Job) error

// FailureHandler reconciles domain state after a failed attempt has been
// committed to the queue (willRetry reports whether the job went back to
// pending). It is not called when the job was cancelled externally.
type FailureHandler func(ctx context.Context, job Job, reason string, willRetry bool) error

// Registry maps job types to handlers. It is safe for concurrent use so
// handlers can be registered by other packages before or after Run starts.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	failures map[string]FailureHandler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}, failures: map[string]FailureHandler{}}
}

// Register sets the handler for jobType. Only registered types are claimed.
func (r *Registry) Register(jobType string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[jobType] = h
}

// RegisterFailure sets the failure reconciliation callback for jobType.
func (r *Registry) RegisterFailure(jobType string, f FailureHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[jobType] = f
}

// Types lists registered handler types in a stable order.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	types := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		types = append(types, t)
	}
	slices.Sort(types)
	return types
}

func (r *Registry) handler(jobType string) Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handlers[jobType]
}

func (r *Registry) failure(jobType string) FailureHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.failures[jobType]
}

// DefaultJobTimeout and JobTimeouts mirror JOB_TIMEOUTS in queue_service.py.
const DefaultJobTimeout = 600 * time.Second

// JobTimeouts is the per-type wall-clock ceiling. Keep in sync with Python.
var JobTimeouts = map[string]time.Duration{
	"run_workbook":               1800 * time.Second,
	"source_workbook":            1800 * time.Second,
	"ambitionbox_import":         900 * time.Second,
	"refresh_workbook":           900 * time.Second,
	"signal_scan":                300 * time.Second,
	"trigger_eval":               600 * time.Second,
	"send":                       300 * time.Second,
	"watch_poll":                 600 * time.Second,
	"source_health_check":        1800 * time.Second,
	"audience_refresh":           900 * time.Second,
	"audience_destination_sync":  1800 * time.Second,
	"research_playbook_run":      3600 * time.Second,
	"research_playbook_schedule": 300 * time.Second,
	"retention_enforce":          1800 * time.Second,
	// Go-only: one plugin run; scrapers wait on per-domain rate limits.
	"plugin_run": 900 * time.Second,
}

// Options configure a Queue. Zero durations take the Python defaults.
type Options struct {
	Concurrency           int
	MaxActivePerWorkspace int
	ShutdownGrace         time.Duration
	// FireKeyPrefix restricts claims to jobs whose fire_key starts with it.
	FireKeyPrefix string

	HeartbeatInterval  time.Duration // 30s
	ClaimCheckInterval time.Duration // 1s: how quickly cancellation is noticed
	ReapInterval       time.Duration // 60s
	StaleAfter         time.Duration // 5m without a heartbeat
	IdlePoll           time.Duration // 1s between empty claims
	ErrorBackoff       time.Duration // 5s after a claim error
	// HandlerStopGrace bounds how long a cancelled handler may take to
	// return before the attempt is finalized without it.
	HandlerStopGrace time.Duration // 5s

	// Timeouts overrides JobTimeouts per type (tests use short ones).
	Timeouts map[string]time.Duration
	WorkerID string
	Logger   *slog.Logger
}

// OptionsFromConfig maps the shared worker configuration.
func OptionsFromConfig(w config.Worker) Options {
	return Options{
		Concurrency:           w.Concurrency,
		MaxActivePerWorkspace: w.MaxActivePerWorkspace,
		ShutdownGrace:         w.ShutdownGrace,
	}
}

func (o *Options) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	o.Concurrency = min(64, max(1, o.Concurrency))
	o.MaxActivePerWorkspace = min(64, max(0, o.MaxActivePerWorkspace))
	o.ShutdownGrace = min(300*time.Second, max(0, o.ShutdownGrace))
	def(&o.HeartbeatInterval, 30*time.Second)
	def(&o.ClaimCheckInterval, time.Second)
	def(&o.ReapInterval, 60*time.Second)
	def(&o.StaleAfter, 5*time.Minute)
	def(&o.IdlePoll, time.Second)
	def(&o.ErrorBackoff, 5*time.Second)
	def(&o.HandlerStopGrace, 5*time.Second)
	if o.WorkerID == "" {
		o.WorkerID = NewWorkerID()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Queue claims and runs Go-routed jobs.
type Queue struct {
	*Registry
	pool   *pgxpool.Pool
	opts   Options
	log    *slog.Logger
	active atomic.Int64
}

// New builds a queue over pool. reg may be nil for a fresh registry.
func New(pool *pgxpool.Pool, reg *Registry, opts Options) *Queue {
	opts.defaults()
	if reg == nil {
		reg = NewRegistry()
	}
	return &Queue{
		Registry: reg,
		pool:     pool,
		opts:     opts,
		log:      opts.Logger.With("component", "queue", "worker_id", opts.WorkerID),
	}
}

// WorkerID is the identity stamped on claimed rows.
func (q *Queue) WorkerID() string { return q.opts.WorkerID }

// NewWorkerID returns hostname:pid:<8 hex>, the format Python uses, so two
// replicas never collide even on one host or after PID reuse.
func NewWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

func (q *Queue) timeoutFor(jobType string) time.Duration {
	if d, ok := q.opts.Timeouts[jobType]; ok {
		return d
	}
	if d, ok := JobTimeouts[jobType]; ok {
		return d
	}
	return DefaultJobTimeout
}

// backoff is 2^exp minutes, capped so the shift cannot overflow a Duration.
func backoff(exp int) time.Duration {
	return time.Minute << min(max(exp, 0), 20)
}
