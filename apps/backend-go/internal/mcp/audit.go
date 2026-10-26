package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const (
	redacted       = "***redacted***"
	maxAuditString = 500
	maxAuditDepth  = 4
	maxAuditError  = 1000
	// maxAuditArgsBytes caps the marshalled redacted arguments of one row.
	maxAuditArgsBytes = 16 << 10
	truncatedMarker   = "***truncated***"
)

// topLevelRedactKeys are the Python _REDACT_KEYS (compared lower-cased).
var topLevelRedactKeys = map[string]bool{
	"token": true, "password": true, "secret": true, "api_key": true, "authorization": true, "email": true,
}

// secretKeyPattern is the broader substring pattern applied at every depth (D6).
// "email" is deliberately absent: nested contact data stays readable.
var secretKeyPattern = regexp.MustCompile(`(?i)token|secret|password|passwd|pwd|authorization|bearer|cookie|credential|session|api[-_]?key|private[-_]?key|access[-_]?key`)

// RedactArguments returns a copy of tool arguments that is safe to persist:
// top-level keys token/password/secret/api_key/authorization/email (any case)
// and, at every depth up to 4, keys matching the broader secret pattern become
// "***redacted***"; strings over 500 characters keep their first 500 plus "…".
// Containers deeper than 4 levels are replaced by a marker, never emitted raw.
// The input is not modified. Deviates from Python (shallow, top-level only): D6.
func RedactArguments(args Args) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if topLevelRedactKeys[strings.ToLower(k)] || secretKeyPattern.MatchString(k) {
			out[k] = redacted
			continue
		}
		out[k] = redactValue(v, 1)
	}
	return out
}

func redactValue(v any, depth int) any {
	switch x := v.(type) {
	case nil, bool, float64, float32, int, int32, int64, uint, uint32, uint64, json.Number:
		return v
	case string:
		return truncateRunes(x, maxAuditString)
	case map[string]any:
		if depth > maxAuditDepth {
			return truncatedMarker
		}
		m := make(map[string]any, len(x))
		for k, e := range x {
			if secretKeyPattern.MatchString(k) {
				m[k] = redacted
				continue
			}
			m[k] = redactValue(e, depth+1)
		}
		return m
	case []any:
		if depth > maxAuditDepth {
			return truncatedMarker
		}
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = redactValue(e, depth+1)
		}
		return l
	default:
		// Typed slices and maps from Go callers: normalise through JSON.
		b, err := json.Marshal(v)
		if err != nil {
			return truncatedMarker
		}
		var n any
		if err := json.Unmarshal(b, &n); err != nil {
			return truncatedMarker
		}
		return redactValue(n, depth)
	}
}

func truncateRunes(s string, n int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if len(s) <= n { // bytes >= runes: cheap exit
		return s
	}
	i, count := 0, 0
	for i < len(s) {
		if count == n {
			return s[:i] + "…"
		}
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
		count++
	}
	return s
}

// AuditOutcome is the result half of one audit row. Status is one of
// ok, denied, error, capped.
type AuditOutcome struct {
	Status         string
	Error          string
	CostUSD        float64
	IdempotencyKey string
}

// auditTimeout bounds one audit write (a var so tests can shorten it).
var auditTimeout = 5 * time.Second

// Record writes one mcp_audit_log row in its own transaction under the caller's
// workspace. It is best effort: a failure is logged ("mcp audit write failed",
// tool and status only, never the arguments) and swallowed, it never panics, and
// it returns within auditTimeout even when the database runner ignores its
// context. The write survives a cancelled request context.
func Record(ctx context.Context, d Deps, c Ctx, tool string, args Args, o AuditOutcome) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp audit write failed", "tool", tool, "status", o.Status, "panic", true)
		}
	}()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- context.Canceled
			}
		}()
		done <- writeAudit(wctx, d, c, tool, args, o)
	}()
	select {
	case err := <-done:
		if err != nil {
			slog.Error("mcp audit write failed", "tool", tool, "status", o.Status, "err", err.Error())
		}
	case <-wctx.Done():
		slog.Error("mcp audit write failed", "tool", tool, "status", o.Status, "err", "timeout")
	}
}

func writeAudit(ctx context.Context, d Deps, c Ctx, tool string, args Args, o AuditOutcome) error {
	raw := auditArgsJSON(args)
	p := gen.McpAuditInsertParams{
		ID:                newUUID(),
		WorkspaceID:       c.WorkspaceID,
		ToolName:          truncateRunes(tool, 100),
		ArgumentsRedacted: raw,
		ResultStatus:      truncateRunes(o.Status, 20),
		EstimatedCostUsd:  o.CostUSD,
	}
	if c.TokenID != "" {
		p.TokenID = pgtype.Text{String: truncateRunes(c.TokenID, 36), Valid: true}
	}
	if c.UserID != nil {
		p.UserID = pgtype.Int4{Int32: *c.UserID, Valid: true}
	}
	if o.IdempotencyKey != "" {
		p.IdempotencyKey = pgtype.Text{String: truncateRunes(o.IdempotencyKey, 128), Valid: true}
	}
	if o.Error != "" {
		p.Error = pgtype.Text{String: truncateRunes(o.Error, maxAuditError), Valid: true}
	}
	return d.RunInWorkspace(ctx, c.WorkspaceID, func(tx pgx.Tx) error {
		return gen.New(tx).McpAuditInsert(ctx, p)
	})
}

// newUUID returns a random RFC 4122 version 4 UUID string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// auditArgsJSON is the redacted arguments as stored; over maxAuditArgsBytes it
// collapses to {"_truncated":true} so one call cannot bloat the audit table.
func auditArgsJSON(args Args) []byte {
	raw, err := json.Marshal(RedactArguments(args))
	if err != nil {
		return []byte("{}")
	}
	if len(raw) > maxAuditArgsBytes {
		return []byte(`{"_truncated":true}`)
	}
	return raw
}
