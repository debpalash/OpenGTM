// Package deststore is the audience destination store (routers/destinations.py,
// services/destinations/models.py and the durable-state half of engine.py):
// destinations, sync runs, the delivery ledger, health, single-flight run start
// with its queue job, and the queue-failure reconcile. Every function runs in the
// caller's transaction, which must be inside tenancy.InWorkspace; the tenant is
// read back from the transaction and every statement also filters on it.
//
// Destination configs hold workspace-secret references only (destvalidate rejects
// credentials), but the store still never logs a config, and Destination.SafeConfig
// is the redacted-on-read form the API returns.
package deststore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Store errors; the routes map them to the Python HTTP answers.
var (
	ErrNotFound         = errors.New("deststore: destination not found")     // 404 "Destination not found"
	ErrRunNotFound      = errors.New("deststore: destination run not found") // 404 "Destination run not found"
	ErrAudienceNotFound = errors.New("deststore: audience not found")        // 404 "Audience not found"
	ErrDuplicate        = errors.New("deststore: destination name taken")    // 409 "A destination with this name already exists for the audience"
	ErrDisabled         = errors.New("deststore: destination is disabled")   // 409 "Destination is disabled"
	ErrNotRetryable     = errors.New("deststore: run not retryable")         // 409 "Only failed or completed-with-errors runs can be retried"
	ErrNotCancellable   = errors.New("deststore: run not cancellable")       // 409 "Only pending or running destination runs can be cancelled"
	errTenant           = errors.New("deststore: workspace does not match the transaction tenant")
	errNoTenant         = errors.New("deststore: the transaction has no workspace (use tenancy.InWorkspace)")
)

// JobType is the queue job type of one destination sync.
const JobType = "audience_destination_sync"

// Destination is an audience_destinations row. Timestamps are naive UTC columns
// rendered as UTC instants; Config and FieldMap are the stored JSON text.
type Destination struct {
	ID, WorkspaceID, AudienceID, Name, Type string
	Enabled                                 bool
	Config, FieldMap                        json.RawMessage
	HealthStatus                            string
	LastError                               *string
	LastSuccessAt                           *time.Time
	CreatedAt, UpdatedAt                    time.Time
}

// SafeConfig is to_api's config: the stored object without its top-level token,
// api_key and secret keys, key order kept. Anything that is not an object is {}.
func (d Destination) SafeConfig() json.RawMessage {
	in := bytes.TrimSpace(d.Config)
	if len(in) == 0 || in[0] != '{' {
		return json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(in))
	if _, err := dec.Token(); err != nil {
		return json.RawMessage(`{}`)
	}
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return json.RawMessage(`{}`)
		}
		key, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return json.RawMessage(`{}`)
		}
		if key == "token" || key == "api_key" || key == "secret" {
			continue
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(key)
		out.Write(kb)
		out.WriteByte(':')
		out.Write(v)
	}
	out.WriteByte('}')
	return out.Bytes()
}

// DestinationWithRun is one row of the destination list with its latest run.
type DestinationWithRun struct {
	Destination
	LatestRun *Run
}

// Run is a destination_runs row.
type Run struct {
	ID, WorkspaceID, DestinationID, Status string
	RequestedBy                            *string
	Attempted, Succeeded, Failed, Skipped  int
	Error                                  *string
	StartedAt, FinishedAt                  *time.Time
	CreatedAt                              time.Time
}

// Delivery is a destination_deliveries ledger row.
type Delivery struct {
	ID                                            int
	WorkspaceID, RunID, DestinationID             string
	LeadID                                        int
	Operation, IdempotencyKey, PayloadFingerprint string
	Payload                                       json.RawMessage
	Status                                        string
	Attempts                                      int
	ExternalID, Summary, Error                    *string
	DeliveredAt                                   *time.Time
	CreatedAt, UpdatedAt                          time.Time
}

func optText(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

func optTime(t pgtype.Timestamp) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func pgText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func destFrom(r gen.AudienceDestination) Destination {
	return Destination{
		ID: r.ID, WorkspaceID: r.WorkspaceID, AudienceID: r.AudienceID, Name: r.Name, Type: r.DestinationType,
		Enabled: r.Enabled, Config: r.Config, FieldMap: r.FieldMap, HealthStatus: r.HealthStatus,
		LastError: optText(r.LastError), LastSuccessAt: optTime(r.LastSuccessAt),
		CreatedAt: r.CreatedAt.Time.UTC(), UpdatedAt: r.UpdatedAt.Time.UTC(),
	}
}

func runFrom(r gen.DestinationRun) Run {
	return Run{
		ID: r.ID, WorkspaceID: r.WorkspaceID, DestinationID: r.DestinationID, Status: r.Status,
		RequestedBy: optText(r.RequestedBy), Attempted: int(r.Attempted), Succeeded: int(r.Succeeded),
		Failed: int(r.Failed), Skipped: int(r.Skipped), Error: optText(r.Error),
		StartedAt: optTime(r.StartedAt), FinishedAt: optTime(r.FinishedAt), CreatedAt: r.CreatedAt.Time.UTC(),
	}
}

func deliveryFrom(r gen.DestinationDelivery) Delivery {
	return Delivery{
		ID: int(r.ID), WorkspaceID: r.WorkspaceID, RunID: r.RunID, DestinationID: r.DestinationID, LeadID: int(r.LeadID),
		Operation: r.Operation, IdempotencyKey: r.IdempotencyKey, PayloadFingerprint: r.PayloadFingerprint,
		Payload: r.Payload, Status: r.Status, Attempts: int(r.Attempts), ExternalID: optText(r.ExternalID),
		Summary: optText(r.Summary), Error: optText(r.Error), DeliveredAt: optTime(r.DeliveredAt),
		CreatedAt: r.CreatedAt.Time.UTC(), UpdatedAt: r.UpdatedAt.Time.UTC(),
	}
}

// tenant returns the transaction's workspace (app.workspace_id).
func tenant(ctx context.Context, tx pgx.Tx) (string, error) {
	var ws string
	if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.workspace_id', true), '')`).Scan(&ws); err != nil {
		return "", fmt.Errorf("deststore: read tenant: %w", err)
	}
	if ws == "" {
		return "", errNoTenant
	}
	return ws, nil
}

// requireTenant fails unless ws is the transaction's workspace.
func requireTenant(ctx context.Context, tx pgx.Tx, ws string) error {
	t, err := tenant(ctx, tx)
	if err != nil {
		return err
	}
	if ws != t {
		return errTenant
	}
	return nil
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
