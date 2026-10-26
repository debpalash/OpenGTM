// Package destinbound ports Python's destinations/inbound.py: the hashed,
// destination-bound machine tokens a CRM uses to call back into OpenGTM, and
// the idempotent reconcile of those callbacks onto leads.
//
// Security: raw tokens come from crypto/rand, only their SHA-256 hex digest is
// stored, a raw token is returned once (RotateResult) and never logged or put
// in an error, and Resolve compares digests in constant time and rechecks
// revocation, destination binding and expiry atomically while stamping use.
package destinbound

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// TokenPrefix starts every raw token.
const TokenPrefix = "dsi_"

// TTLEnv is the environment variable holding the token lifetime in days.
const TTLEnv = "OPENGTM_DESTINATION_TOKEN_TTL_DAYS"

// ErrInvalidToken is the single failure of Resolve; its text is the 401 detail.
// Unknown, expired, revoked and foreign-destination tokens are indistinguishable.
var ErrInvalidToken = errors.New("invalid or revoked destination token")

// HashToken is sha256 hex of the raw bearer.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// GenerateToken returns a new raw token ("dsi_" + 32 crypto/rand bytes,
// base64url without padding), its hex SHA-256 and its 12 character prefix.
func GenerateToken() (raw, hash, prefix string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("destinbound: crypto/rand failed: " + err.Error())
	}
	raw = TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return raw, HashToken(raw), raw[:12]
}

// TokenTTL is the token lifetime for a configured day count, clamped to 1-365 days.
func TokenTTL(envDays int) time.Duration {
	if envDays < 1 {
		envDays = 1
	}
	if envDays > 365 {
		envDays = 365
	}
	return time.Duration(envDays) * 24 * time.Hour
}

// TTLDays parses the env value like int(os.getenv(..., "90")) with a fallback
// to 90 on any ValueError (and for an unset or empty variable).
func TTLDays(s string) int {
	v, ok := parsePyInt(s)
	if !ok {
		return 90
	}
	if !v.IsInt64() || v.Int64() > math.MaxInt32 {
		if v.Sign() < 0 {
			return 1
		}
		return 365
	}
	n := v.Int64()
	if n < math.MinInt32 {
		return 1
	}
	return int(n)
}

// parsePyInt is Python's int(str): surrounding whitespace, a sign, base 10
// digits of any Unicode decimal script, single underscores between digits.
func parsePyInt(s string) (*big.Int, bool) {
	s = strings.TrimFunc(s, unicode.IsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return nil, false
	}
	var digits []byte
	prevUnderscore := true // forbids a leading underscore
	for _, r := range s {
		if r == '_' {
			if prevUnderscore {
				return nil, false
			}
			prevUnderscore = true
			continue
		}
		if !unicode.Is(unicode.Nd, r) {
			return nil, false
		}
		start := r
		for unicode.Is(unicode.Nd, start-1) {
			start--
		}
		digits = append(digits, byte('0'+(r-start)%10))
		prevUnderscore = false
	}
	if prevUnderscore {
		return nil, false
	}
	v, ok := new(big.Int).SetString(string(digits), 10)
	if !ok {
		return nil, false
	}
	if neg {
		v.Neg(v)
	}
	return v, true
}

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("destinbound: crypto/rand failed: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// requireTenant fails unless the transaction's app.workspace_id is ws.
func requireTenant(ctx context.Context, tx pgx.Tx, ws string) error {
	if ws == "" {
		return errors.New("destinbound: a workspace is required")
	}
	var txWS string
	if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.workspace_id', true), '')`).Scan(&txWS); err != nil {
		return fmt.Errorf("destinbound: read tenant: %w", err)
	}
	if txWS != ws {
		return errors.New("destinbound: workspace does not match transaction tenant")
	}
	return nil
}

// RotateResult is the one-time reveal of a new token.
type RotateResult struct {
	Token     string // raw; shown once, never stored
	Prefix    string
	CreatedAt time.Time // naive UTC column
	ExpiresAt time.Time
}

// Rotate revokes the destination's active tokens and inserts a fresh one that
// expires now + the configured TTL. Run inside tenancy.InWorkspace(wsID); the
// caller has already loaded the destination for wsID and checked its type.
func Rotate(ctx context.Context, tx pgx.Tx, wsID, destID string, createdBy int32, now time.Time) (RotateResult, error) {
	if err := requireTenant(ctx, tx, wsID); err != nil {
		return RotateResult{}, err
	}
	q := gen.New(tx)
	if _, err := q.DestInboundTokensRevokeActive(ctx, gen.DestInboundTokensRevokeActiveParams{
		Now: tstz(now), WorkspaceID: wsID, DestinationID: destID}); err != nil {
		return RotateResult{}, fmt.Errorf("destinbound: revoke previous: %w", err)
	}
	raw, hash, prefix := GenerateToken()
	expires := now.UTC().Add(TokenTTL(TTLDays(os.Getenv(TTLEnv))))
	createdAt, err := q.DestInboundTokenInsert(ctx, gen.DestInboundTokenInsertParams{
		ID: newID(), WorkspaceID: wsID, DestinationID: destID, TokenHash: hash, Prefix: prefix,
		CreatedBy: pgtype.Int4{Int32: createdBy, Valid: true}, ExpiresAt: tstz(expires)})
	if err != nil {
		return RotateResult{}, fmt.Errorf("destinbound: insert token: %w", err)
	}
	return RotateResult{Token: raw, Prefix: prefix, CreatedAt: createdAt.Time, ExpiresAt: expires}, nil
}

// TokenStatus is the inbound-token status. Has is false when no unrevoked
// token exists (JSON {"active": false}).
type TokenStatus struct {
	Has        bool
	Active     bool
	Expired    bool
	Prefix     string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedBy  *int32
}

// Status reports the newest unrevoked token of the destination.
func Status(ctx context.Context, tx pgx.Tx, wsID, destID string, now time.Time) (TokenStatus, error) {
	if err := requireTenant(ctx, tx, wsID); err != nil {
		return TokenStatus{}, err
	}
	row, err := gen.New(tx).DestInboundTokenLatestActive(ctx, gen.DestInboundTokenLatestActiveParams{
		WorkspaceID: wsID, DestinationID: destID})
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenStatus{}, nil
	}
	if err != nil {
		return TokenStatus{}, fmt.Errorf("destinbound: status: %w", err)
	}
	st := TokenStatus{Has: true, Prefix: row.Prefix, CreatedAt: row.CreatedAt.Time}
	live := false
	if row.ExpiresAt.Valid {
		e := row.ExpiresAt.Time.UTC()
		st.ExpiresAt = &e
		live = e.After(now)
	}
	if row.LastUsedAt.Valid {
		l := row.LastUsedAt.Time.UTC()
		st.LastUsedAt = &l
	}
	if row.CreatedBy.Valid {
		c := row.CreatedBy.Int32
		st.CreatedBy = &c
	}
	st.Active, st.Expired = live, !live
	return st, nil
}

// Revoke revokes every active token of the destination and returns how many.
func Revoke(ctx context.Context, tx pgx.Tx, wsID, destID string, now time.Time) (int64, error) {
	if err := requireTenant(ctx, tx, wsID); err != nil {
		return 0, err
	}
	n, err := gen.New(tx).DestInboundTokensRevokeActive(ctx, gen.DestInboundTokensRevokeActiveParams{
		Now: tstz(now), WorkspaceID: wsID, DestinationID: destID})
	if err != nil {
		return 0, fmt.Errorf("destinbound: revoke: %w", err)
	}
	return n, nil
}

// Verified is a bearer that passed every check of Lookup and has not been stamped yet.
type Verified struct {
	WorkspaceID string
	tokenID     string
	destID      string
	now         time.Time
}

// Resolve authenticates a raw bearer for destID and returns its workspace. It
// runs on the plain pool (the table is not RLS). Every authentication failure
// is ErrInvalidToken; only infrastructure errors differ (fail closed).
func Resolve(ctx context.Context, pool *pgxpool.Pool, raw, destID string, now time.Time) (string, error) {
	v, err := Lookup(ctx, pool, raw, destID, now)
	if err != nil {
		return "", err
	}
	return v.Stamp(ctx, pool)
}

// Lookup is the read-only half of Resolve: it verifies the bearer without writing, so a caller can
// apply its own budget before the last_used_at stamp (an over-budget token then costs no write).
func Lookup(ctx context.Context, pool *pgxpool.Pool, raw, destID string, now time.Time) (*Verified, error) {
	if raw == "" {
		return nil, ErrInvalidToken
	}
	digest := HashToken(raw)
	row, err := gen.New(pool).DestInboundTokenByHash(ctx, digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("destinbound: resolve: %w", err)
	}
	okHash := subtle.ConstantTimeCompare([]byte(row.TokenHash), []byte(digest)) == 1
	okDest := subtle.ConstantTimeCompare([]byte(row.DestinationID), []byte(destID)) == 1
	if !okHash || !okDest || row.RevokedAt.Valid || !row.ExpiresAt.Valid || !row.ExpiresAt.Time.After(now) {
		return nil, ErrInvalidToken
	}
	return &Verified{WorkspaceID: row.WorkspaceID, tokenID: row.ID, destID: destID, now: now}, nil
}

// Stamp records the use atomically (rechecking revoked, expiry and the destination binding) and
// returns the token's workspace.
func (v *Verified) Stamp(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	ws, err := gen.New(pool).DestInboundTokenStamp(ctx, gen.DestInboundTokenStampParams{
		Now: tstz(v.now), ID: v.tokenID, DestinationID: v.destID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidToken
	}
	if err != nil {
		return "", fmt.Errorf("destinbound: stamp: %w", err)
	}
	return ws, nil
}

// pyISO renders like datetime.isoformat() (microseconds only when non-zero).
func pyISO(t time.Time, aware bool) string {
	s := t.UTC().Format("2006-01-02T15:04:05")
	if us := t.UTC().Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	if aware {
		s += "+00:00"
	}
	return s
}
