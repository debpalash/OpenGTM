package govidp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const defaultTTLDays = 90

// TokenStatus is scim.token_status (JSON key order as Python's dict).
type TokenStatus struct {
	TokenPrefix string   `json:"token_prefix"`
	CreatedAt   float64  `json:"created_at"`
	ExpiresAt   float64  `json:"expires_at"`
	LastUsedAt  *float64 `json:"last_used_at"`
	CreatedBy   *int32   `json:"created_by"`
	Expired     bool     `json:"expired"`
}

// ttlDays is scim._token_ttl_seconds: OPENGTM_SCIM_TOKEN_TTL_DAYS read per
// call, non-integer -> 90, clamped to 1..365.
func ttlDays() int {
	days, err := strconv.Atoi(strings.TrimSpace(os.Getenv("OPENGTM_SCIM_TOKEN_TTL_DAYS")))
	if err != nil {
		days = defaultTTLDays
	}
	return min(max(days, 1), 365)
}

func ttlSeconds() float64 { return float64(ttlDays()) * 86400 }

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Rotate is scim.rotate_token: a fresh og_scim_ token (36 CSPRNG bytes, URL-safe
// base64 without padding), stored only as its sha256 hex digest plus the first
// 16 characters as display prefix; last_used_at is cleared, so the previous
// token stops authenticating. The plaintext is returned once and never logged.
func Rotate(ctx context.Context, tx pgx.Tx, ws string, by int32, now time.Time) (string, error) {
	buf := make([]byte, 36)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("govidp: token generation failed")
	}
	token := "og_scim_" + base64.RawURLEncoding.EncodeToString(buf)
	created := epoch(now)
	err := gen.New(tx).GovIdpRotateToken(ctx, gen.GovIdpRotateTokenParams{
		WorkspaceID: ws, TokenHash: digest(token), TokenPrefix: token[:16],
		CreatedAt: created, ExpiresAt: created + ttlSeconds(), CreatedBy: by,
	})
	if err != nil {
		return "", errors.New("govidp: storing scim token failed")
	}
	return token, nil
}

// Revoke is scim.revoke_token (idempotent).
func Revoke(ctx context.Context, tx pgx.Tx, ws string) error {
	return gen.New(tx).GovIdpRevokeToken(ctx, ws)
}

// Status is scim.token_status; nil when the workspace has no token. A NULL or
// zero expires_at falls back to created_at + TTL (Python `or`); expired is
// expires_at <= now.
func Status(ctx context.Context, tx pgx.Tx, ws string, now time.Time) (*TokenStatus, error) {
	row, err := gen.New(tx).GovIdpTokenStatus(ctx, ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st := &TokenStatus{TokenPrefix: row.TokenPrefix, CreatedAt: row.CreatedAt}
	st.ExpiresAt = st.CreatedAt + ttlSeconds()
	if row.ExpiresAt.Valid && row.ExpiresAt.Float64 != 0 {
		st.ExpiresAt = row.ExpiresAt.Float64
	}
	st.LastUsedAt = f8(row.LastUsedAt)
	if row.CreatedBy.Valid {
		v := row.CreatedBy.Int32
		st.CreatedBy = &v
	}
	st.Expired = st.ExpiresAt <= epoch(now)
	return st, nil
}

func f8(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}
