package destinbound

import (
	"encoding/json"
	"time"
)

// MarshalJSON renders the rotate response, including the one-time warning.
func (r RotateResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"token": r.Token, "prefix": r.Prefix,
		"created_at": pyISO(r.CreatedAt, false), "expires_at": pyISO(r.ExpiresAt, true),
		"warning": "Copy this token now; it will not be shown again.",
	})
}

// MarshalJSON renders {"active": false} or the full status shape.
func (s TokenStatus) MarshalJSON() ([]byte, error) {
	if !s.Has {
		return []byte(`{"active":false}`), nil
	}
	aware := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return pyISO(*t, true)
	}
	return json.Marshal(map[string]any{
		"active": s.Active, "expired": s.Expired, "prefix": s.Prefix,
		"created_at": pyISO(s.CreatedAt, false), "expires_at": aware(s.ExpiresAt),
		"last_used_at": aware(s.LastUsedAt), "created_by": s.CreatedBy,
	})
}
