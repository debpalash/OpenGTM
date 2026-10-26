package audiencerefresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Audience is Audience.to_api (key order kept; naive datetimes as isoformat).
type Audience struct {
	ID                         string          `json:"id"`
	Name                       string          `json:"name"`
	Description                string          `json:"description"`
	Filters                    json.RawMessage `json:"filters"`
	MemberCount                int             `json:"member_count"`
	CreatedAt                  *string         `json:"created_at"`
	UpdatedAt                  *string         `json:"updated_at"`
	RefreshedAt                *string         `json:"refreshed_at"`
	RefreshEnabled             bool            `json:"refresh_enabled"`
	RefreshIntervalMinutes     int             `json:"refresh_interval_minutes"`
	NextRefreshAt              *string         `json:"next_refresh_at"`
	RefreshHealth              string          `json:"refresh_health"`
	LastRefreshError           *string         `json:"last_refresh_error"`
	ConsecutiveRefreshFailures int             `json:"consecutive_refresh_failures"`
}

func iso(t pgtype.Timestamp) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.Format("2006-01-02T15:04:05")
	if us := t.Time.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return &s
}

func readAudience(ctx context.Context, pool *pgxpool.Pool, ws, id string) (Audience, error) {
	var a Audience
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		r, err := gen.New(tx).AudRefreshGet(ctx, gen.AudRefreshGetParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("audiencerefresh: read audience: %w", err)
		}
		filters := json.RawMessage(r.Filters)
		if len(filters) == 0 || string(filters) == "null" {
			filters = json.RawMessage(`{}`)
		}
		a = Audience{ID: r.ID, Name: r.Name, Description: r.Description, Filters: filters, MemberCount: int(r.MemberCount),
			CreatedAt: iso(r.CreatedAt), UpdatedAt: iso(r.UpdatedAt), RefreshedAt: iso(r.RefreshedAt),
			RefreshEnabled: r.RefreshEnabled, RefreshIntervalMinutes: int(r.RefreshIntervalMinutes), NextRefreshAt: iso(r.NextRefreshAt),
			RefreshHealth: r.RefreshHealth, ConsecutiveRefreshFailures: int(r.ConsecutiveRefreshFailures)}
		if r.LastRefreshError.Valid {
			s := r.LastRefreshError.String
			a.LastRefreshError = &s
		}
		if a.RefreshIntervalMinutes == 0 {
			a.RefreshIntervalMinutes = 60
		}
		if a.RefreshHealth == "" {
			a.RefreshHealth = "unverified"
		}
		return nil
	})
	return a, err
}

// pyTypeName is the Python exception class name of a refresh failure.
func pyTypeName(err error) string {
	var t PyTyper
	switch {
	case errors.As(err, &t):
		return t.PyType()
	case errors.Is(err, context.DeadlineExceeded):
		return "TimeoutError"
	case errors.Is(err, context.Canceled):
		return "CancelledError"
	}
	return "RuntimeError"
}

// failureText is f"{type(exc).__name__}: {exc}"[:1000] (characters, not bytes).
func failureText(err error) string {
	r := []rune(pyTypeName(err) + ": " + err.Error())
	if len(r) > 1000 {
		r = r[:1000]
	}
	return string(r)
}

// RefreshObserved is refresh_audience_observed: a failed refresh has already rolled
// back, so health is stored in a fresh transaction (even when ctx was cancelled) and
// the error returned; success stores healthy.
func RefreshObserved(ctx context.Context, pool *pgxpool.Pool, wsID, audienceID string, deps Deps) (Result, error) {
	deps = deps.withDefaults()
	rctx := ctx
	if deps.Timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, deps.Timeout)
		defer cancel()
	}
	res, err := refresh(rctx, pool, wsID, audienceID, deps)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, tenancy.ErrNoWorkspace) {
			return Result{}, err
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		herr := tenancy.InWorkspace(sctx, pool, wsID, func(tx pgx.Tx) error {
			_, e := gen.New(tx).AudRefreshMarkDegraded(sctx, gen.AudRefreshMarkDegradedParams{
				ID: audienceID, WorkspaceID: wsID, LastError: failureText(err)})
			return e
		})
		if herr != nil && !errors.Is(herr, pgx.ErrNoRows) {
			return Result{}, errors.Join(err, fmt.Errorf("audiencerefresh: record degraded health: %w", herr))
		}
		return Result{}, err
	}
	err = tenancy.InWorkspace(ctx, pool, wsID, func(tx pgx.Tx) error {
		return gen.New(tx).AudRefreshMarkHealthy(ctx, gen.AudRefreshMarkHealthyParams{ID: audienceID, WorkspaceID: wsID})
	})
	if err != nil {
		return Result{}, fmt.Errorf("audiencerefresh: record healthy: %w", err)
	}
	if res.Audience, err = readAudience(ctx, pool, wsID, audienceID); err != nil {
		return Result{}, err
	}
	return res, nil
}
