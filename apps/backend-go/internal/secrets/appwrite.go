package secrets

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// SetAppSetting upserts a global app_settings row (plain pool; no RLS). The
// caller decides whether value is ciphertext or plaintext.
func (s *PgStore) SetAppSetting(ctx context.Context, key, value string) error {
	return gen.New(s.pool).UpsertAppSetting(ctx, gen.UpsertAppSettingParams{Key: key, Value: value})
}
