// Package chathistory stores copilot conversations, messages and tool
// approvals on the chat_* tables (port of services/chat_history.py, PG path).
// Every function takes a pgx.Tx that must come from tenancy.InWorkspace.
package chathistory

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// SelfHostUser is Python's SELF_HOST_USER partition for user_id=None.
const SelfHostUser = "self-host"

// MaxContentBytes caps message content and tool_data (1 MiB).
const MaxContentBytes = 1 << 20

var (
	ErrInvalidRole     = errors.New("chathistory: invalid message role")
	ErrContentTooLarge = errors.New("chathistory: message content too large")
	ErrNoConversation  = errors.New("chathistory: conversation not found")
	ErrNoWorkspace     = errors.New("chathistory: workspace required")
)

// Conversation JSON shape and key order follow Python's dict(row).
type Conversation struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Message JSON shape and key order follow Python's _pg_get_messages.
type Message struct {
	ID             string  `json:"id"`
	ConversationID string  `json:"conversation_id"`
	Role           string  `json:"role"`
	Content        string  `json:"content"`
	ToolData       *string `json:"tool_data"`
	CreatedAt      string  `json:"created_at"`
}

// uidString is Python's _uid: nil (self-host, no auth) maps to SelfHostUser.
func uidString(uid *int32) string {
	if uid == nil {
		return SelfHostUser
	}
	return itoa(int64(*uid))
}

func itoa(n int64) string {
	b := [20]byte{}
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Create starts a conversation; like Python, timestamps in the result are the
// client-side UTC isoformat, not re-read from the row.
func Create(ctx context.Context, tx pgx.Tx, ws string, uid *int32, title string) (Conversation, error) {
	if ws == "" {
		return Conversation{}, ErrNoWorkspace
	}
	id := uuid.NewString()[:12]
	err := gen.New(tx).ChatCreateConversation(ctx, gen.ChatCreateConversationParams{
		ID: id, Title: title, WorkspaceID: ws, UserID: uidString(uid),
	})
	if err != nil {
		return Conversation{}, err
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	return Conversation{ID: id, Title: title, CreatedAt: now, UpdatedAt: now}, nil
}

func List(ctx context.Context, tx pgx.Tx, ws string, uid *int32, limit int) ([]Conversation, error) {
	rows, err := gen.New(tx).ChatListConversations(ctx, gen.ChatListConversationsParams{
		WorkspaceID: ws, UserID: uidString(uid), RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Conversation, 0, len(rows))
	for _, r := range rows {
		out = append(out, Conversation{r.ID, r.Title, r.CreatedAt, r.UpdatedAt})
	}
	return out, nil
}

// Get returns nil, nil when the conversation is unknown or not owned.
func Get(ctx context.Context, tx pgx.Tx, id, ws string, uid *int32) (*Conversation, error) {
	r, err := gen.New(tx).ChatGetConversation(ctx, gen.ChatGetConversationParams{
		ID: id, WorkspaceID: ws, UserID: uidString(uid),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Conversation{r.ID, r.Title, r.CreatedAt, r.UpdatedAt}, nil
}

func Messages(ctx context.Context, tx pgx.Tx, convID, ws string, uid *int32) ([]Message, error) {
	rows, err := gen.New(tx).ChatGetMessages(ctx, gen.ChatGetMessagesParams{
		ConversationID: convID, WorkspaceID: ws, UserID: uidString(uid),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(rows))
	for _, r := range rows {
		m := Message{ID: r.ID, ConversationID: r.ConversationID, Role: r.Role, Content: r.Content, CreatedAt: r.CreatedAt}
		if r.ToolData.Valid {
			s := r.ToolData.String
			m.ToolData = &s
		}
		out = append(out, m)
	}
	return out, nil
}

// AddMessage appends a message and bumps updated_at. uid is the caller's
// authenticated user (nil = self-host): the query is stricter than Python and
// requires the conversation to belong to (ws, uid). Callers should insert the
// user and assistant messages in separate transactions so created_at differs.
func AddMessage(ctx context.Context, tx pgx.Tx, convID, role, content string, toolData *string, ws string, uid *int32) error {
	if ws == "" {
		return ErrNoWorkspace
	}
	switch role {
	case "user", "assistant", "system", "tool":
	default:
		return ErrInvalidRole
	}
	if len(content) > MaxContentBytes || (toolData != nil && len(*toolData) > MaxContentBytes) {
		return ErrContentTooLarge
	}
	td := pgtype.Text{}
	if toolData != nil {
		td = pgtype.Text{String: *toolData, Valid: true}
	}
	_, err := gen.New(tx).ChatAddMessage(ctx, gen.ChatAddMessageParams{
		ID: uuid.NewString()[:12], Role: role, Content: content, ToolData: td,
		ConversationID: convID, WorkspaceID: ws, UserID: uidString(uid),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoConversation
	}
	return err
}

func UpdateTitle(ctx context.Context, tx pgx.Tx, convID, title, ws string, uid *int32) error {
	return gen.New(tx).ChatUpdateTitle(ctx, gen.ChatUpdateTitleParams{
		ID: convID, Title: title, WorkspaceID: ws, UserID: uidString(uid),
	})
}

// Delete is a silent no-op for unknown or foreign conversations.
func Delete(ctx context.Context, tx pgx.Tx, convID, ws string, uid *int32) error {
	return gen.New(tx).ChatDeleteConversation(ctx, gen.ChatDeleteConversationParams{
		ID: convID, WorkspaceID: ws, UserID: uidString(uid),
	})
}
