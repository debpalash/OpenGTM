// Package outreachstore is the tenant-scoped outreach store (port of
// services/outreach/store.py). Every function takes a pgx.Tx already scoped by
// tenancy.InWorkspace; statements also filter workspace_id (belt) on top of
// FORCE RLS (suspenders).
package outreachstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Seq is _seq_to_dict: the field order below is the Python dict key order.
type Seq struct {
	ID              string          `json:"id"`
	WorkspaceID     string          `json:"workspace_id"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Steps           json.RawMessage `json:"steps"`
	Status          *string         `json:"status"`
	DailyLimit      *int            `json:"daily_limit"`
	SendWindowStart *int            `json:"send_window_start"`
	SendWindowEnd   *int            `json:"send_window_end"`
	SendWindowTZ    *string         `json:"send_window_tz"`
	ConsentBasis    string          `json:"consent_basis"`
	BounceCount     int             `json:"bounce_count"`
	ComplaintCount  int             `json:"complaint_count"`
	AutoPaused      bool            `json:"auto_paused"`
	CreatedAt       *string         `json:"created_at"`
	UpdatedAt       *string         `json:"updated_at"`
}

// SeqInput is create_sequence's argument list. Steps is the JSON text stored
// verbatim (nil stores []).
type SeqInput struct {
	Name            string
	Description     string
	Steps           json.RawMessage
	DailyLimit      int
	SendWindowStart int
	SendWindowEnd   int
	SendWindowTZ    string
	ConsentBasis    string
}

// NewSeqInput returns create_sequence's Python defaults (50/day, 9-18 UTC).
func NewSeqInput(name string) SeqInput {
	return SeqInput{Name: name, DailyLimit: 50, SendWindowStart: 9, SendWindowEnd: 18, SendWindowTZ: "UTC"}
}

// isoPy renders datetime.isoformat() for an aware UTC value: microseconds only
// when non-zero, "+00:00" offset.
func isoPy(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	var s string
	if u.Nanosecond()/1000 == 0 {
		s = u.Format("2006-01-02T15:04:05") + "+00:00"
	} else {
		s = u.Format("2006-01-02T15:04:05.000000") + "+00:00"
	}
	return &s
}

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func intPtr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

func textPtr(v pgtype.Text) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func int4Or0(v pgtype.Int4) int { return int(v.Int32) }

// stepsOrEmpty is `row.steps or []`: a falsy stored value (null, [], {}, "",
// 0, false) becomes [].
func stepsOrEmpty(raw string) json.RawMessage {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return json.RawMessage(raw)
	}
	switch x := v.(type) {
	case nil:
		return json.RawMessage(`[]`)
	case bool:
		if !x {
			return json.RawMessage(`[]`)
		}
	case float64:
		if x == 0 {
			return json.RawMessage(`[]`)
		}
	case string:
		if x == "" {
			return json.RawMessage(`[]`)
		}
	case []any:
		if len(x) == 0 {
			return json.RawMessage(`[]`)
		}
	case map[string]any:
		if len(x) == 0 {
			return json.RawMessage(`[]`)
		}
	}
	return json.RawMessage(raw)
}

// seqRow is the column set every sequence query returns.
type seqRow = gen.OutreachSeqGetRow

func toSeq(r seqRow) Seq {
	return Seq{
		ID: r.ID, WorkspaceID: r.WorkspaceID, Name: r.Name, Description: r.Description.String,
		Steps: stepsOrEmpty(r.Steps), Status: textPtr(r.Status), DailyLimit: intPtr(r.DailyLimit),
		SendWindowStart: intPtr(r.SendWindowStart), SendWindowEnd: intPtr(r.SendWindowEnd),
		SendWindowTZ: textPtr(r.SendWindowTz), ConsentBasis: r.ConsentBasis,
		BounceCount: int4Or0(r.BounceCount), ComplaintCount: int4Or0(r.ComplaintCount),
		AutoPaused: r.AutoPaused.Valid && r.AutoPaused.Bool,
		CreatedAt:  isoPy(r.CreatedAt), UpdatedAt: isoPy(r.UpdatedAt),
	}
}

// CreateSequence inserts a draft sequence with a fresh uuid4 id.
func CreateSequence(ctx context.Context, tx pgx.Tx, wsID string, in SeqInput) (Seq, error) {
	steps := string(in.Steps)
	if steps == "" {
		steps = "[]"
	}
	tz := in.SendWindowTZ
	if tz == "" {
		tz = "UTC"
	}
	daily, err := asInt32("daily_limit", in.DailyLimit)
	if err != nil {
		return Seq{}, err
	}
	start, err := asInt32("send_window_start", in.SendWindowStart)
	if err != nil {
		return Seq{}, err
	}
	end, err := asInt32("send_window_end", in.SendWindowEnd)
	if err != nil {
		return Seq{}, err
	}
	r, err := gen.New(tx).OutreachSeqCreate(ctx, gen.OutreachSeqCreateParams{
		ID: uuid.NewString(), WorkspaceID: wsID, Name: in.Name, Description: in.Description, Steps: steps,
		DailyLimit: daily, SendWindowStart: start,
		SendWindowEnd: end, SendWindowTz: tz, ConsentBasis: in.ConsentBasis,
	})
	if err != nil {
		return Seq{}, fmt.Errorf("outreachstore: create sequence: %w", err)
	}
	return toSeq(seqRow(r)), nil
}

// ListSequences returns the workspace's sequences, newest first.
func ListSequences(ctx context.Context, tx pgx.Tx, wsID string) ([]Seq, error) {
	rows, err := gen.New(tx).OutreachSeqList(ctx, wsID)
	if err != nil {
		return nil, fmt.Errorf("outreachstore: list sequences: %w", err)
	}
	out := make([]Seq, 0, len(rows))
	for _, r := range rows {
		out = append(out, toSeq(seqRow(r)))
	}
	return out, nil
}

// GetSequence returns nil when the id is unknown in this workspace.
func GetSequence(ctx context.Context, tx pgx.Tx, wsID, id string) (*Seq, error) {
	r, err := gen.New(tx).OutreachSeqGet(ctx, gen.OutreachSeqGetParams{WorkspaceID: wsID, ID: id})
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outreachstore: get sequence: %w", err)
	}
	s := toSeq(r)
	return &s, nil
}

// SequenceExists is the in-workspace anti-orphan check.
func SequenceExists(ctx context.Context, tx pgx.Tx, wsID, id string) (bool, error) {
	ok, err := gen.New(tx).OutreachSeqExists(ctx, gen.OutreachSeqExistsParams{WorkspaceID: wsID, ID: id})
	if err != nil {
		return false, fmt.Errorf("outreachstore: sequence exists: %w", err)
	}
	return ok, nil
}

func asInt32(k string, v any) (int32, error) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int32:
		n = int64(x)
	case int64:
		n = x
	case float64:
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("outreachstore: %s must be an integer", k)
		}
		n = int64(x)
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0, fmt.Errorf("outreachstore: %s must be an integer", k)
		}
		n = i
	default:
		return 0, fmt.Errorf("outreachstore: %s must be an integer", k)
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("outreachstore: %s out of range", k)
	}
	return int32(n), nil
}

func asString(k string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("outreachstore: %s must be a string", k)
	}
	return s, nil
}

func asJSONText(k string, v any) (string, error) {
	switch x := v.(type) {
	case json.RawMessage:
		return string(x), nil
	case []byte:
		return string(x), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("outreachstore: %s is not JSON: %w", k, err)
	}
	return string(b), nil
}

// UpdateSequence applies only the updatable keys present in updates (Python
// _UPDATABLE_SEQ_FIELDS over model_dump(exclude_none=True)); other keys are
// ignored. Callers must drop nil values first (model_dump(exclude_none=True));
// a nil value is a type error, not a stored NULL. Returns nil when the sequence is unknown in this workspace. With no
// updatable key the row is returned untouched.
func UpdateSequence(ctx context.Context, tx pgx.Tx, wsID, id string, updates map[string]any) (*Seq, error) {
	p := gen.OutreachSeqUpdateParams{WorkspaceID: wsID, ID: id}
	any_ := false
	var err error
	for k, v := range updates {
		switch k {
		case "name":
			p.SetName = true
			p.Name, err = asString(k, v)
		case "description":
			p.SetDescription = true
			p.Description, err = asString(k, v)
		case "steps":
			p.SetSteps = true
			p.Steps, err = asJSONText(k, v)
		case "status":
			p.SetStatus = true
			p.Status, err = asString(k, v)
		case "daily_limit":
			p.SetDailyLimit = true
			p.DailyLimit, err = asInt32(k, v)
		case "send_window_start":
			p.SetSendWindowStart = true
			p.SendWindowStart, err = asInt32(k, v)
		case "send_window_end":
			p.SetSendWindowEnd = true
			p.SendWindowEnd, err = asInt32(k, v)
		case "send_window_tz":
			p.SetSendWindowTz = true
			p.SendWindowTz, err = asString(k, v)
		case "consent_basis":
			p.SetConsentBasis = true
			p.ConsentBasis, err = asString(k, v)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		any_ = true
	}
	if !any_ {
		return GetSequence(ctx, tx, wsID, id)
	}
	r, err := gen.New(tx).OutreachSeqUpdate(ctx, p)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outreachstore: update sequence: %w", err)
	}
	s := toSeq(seqRow(r))
	return &s, nil
}

// SetSequenceStatus is set_sequence_status.
func SetSequenceStatus(ctx context.Context, tx pgx.Tx, wsID, id, status string) (*Seq, error) {
	return UpdateSequence(ctx, tx, wsID, id, map[string]any{"status": status})
}

// DeleteSequence removes the sequence with its sends, enrollments and schedule
// mirror row (delete_sequence). Reports false when the id is unknown here.
func DeleteSequence(ctx context.Context, tx pgx.Tx, wsID, id string) (bool, error) {
	ok, err := SequenceExists(ctx, tx, wsID, id)
	if err != nil || !ok {
		return false, err
	}
	q := gen.New(tx)
	if err := q.OutreachSeqDeleteSends(ctx, gen.OutreachSeqDeleteSendsParams{WorkspaceID: wsID, SequenceID: id}); err != nil {
		return false, fmt.Errorf("outreachstore: delete sends: %w", err)
	}
	if err := q.OutreachSeqDeleteEnrollments(ctx, gen.OutreachSeqDeleteEnrollmentsParams{WorkspaceID: wsID, SequenceID: id}); err != nil {
		return false, fmt.Errorf("outreachstore: delete enrollments: %w", err)
	}
	if _, err := q.OutreachSeqDelete(ctx, gen.OutreachSeqDeleteParams{WorkspaceID: wsID, ID: id}); err != nil {
		return false, fmt.Errorf("outreachstore: delete sequence: %w", err)
	}
	if err := q.OutreachSeqDeleteSchedule(ctx, gen.OutreachSeqDeleteScheduleParams{WorkspaceID: wsID, SequenceID: id}); err != nil {
		return false, fmt.Errorf("outreachstore: delete schedule: %w", err)
	}
	return true, nil
}
