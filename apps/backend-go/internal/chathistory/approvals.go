package chathistory

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// CreateApproval stores a pending tool call (json.dumps sort_keys, compact,
// ensure_ascii) and returns the opaque approval id (uuid4 hex).
func CreateApproval(ctx context.Context, tx pgx.Tx, ws string, uid *int32, toolCall map[string]any) (string, error) {
	if ws == "" {
		return "", ErrNoWorkspace
	}
	b, err := canonjson.Marshal(toolCall, true)
	if err != nil {
		return "", err
	}
	id := uuid.New().String()
	id = id[:8] + id[9:13] + id[14:18] + id[19:23] + id[24:]
	err = gen.New(tx).ChatCreateApproval(ctx, gen.ChatCreateApprovalParams{
		ID: id, WorkspaceID: ws, UserID: uidString(uid),
		ToolCallJson: string(b), CreatedAtEpoch: time.Now().Unix(),
	})
	return id, err
}

// ConsumeApproval atomically flips a pending, in-window, tenant-matching
// approval to decision and returns the stored call with id replaced by the
// approval id. Invalid decision, empty ids, or no matching row -> nil,false.
func ConsumeApproval(ctx context.Context, tx pgx.Tx, id, ws string, uid *int32, decision string, ttl time.Duration, now time.Time) (map[string]any, bool, error) {
	if (decision != "approve" && decision != "deny") || id == "" || ws == "" {
		return nil, false, nil
	}
	raw, err := gen.New(tx).ChatConsumeApproval(ctx, gen.ChatConsumeApprovalParams{
		Decision: decision, ID: id, WorkspaceID: ws, UserID: uidString(uid),
		MinCreatedAtEpoch: now.Unix() - int64(ttl/time.Second),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var call map[string]any
	if err := json.Unmarshal([]byte(raw), &call); err != nil || call == nil {
		return nil, false, nil
	}
	call["id"] = id
	return call, true, nil
}
