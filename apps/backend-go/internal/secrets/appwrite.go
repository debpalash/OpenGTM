package secrets

import (
	"context"
	"sort"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// SetAppSetting upserts a global app_settings row (plain pool; no RLS). The
// caller decides whether value is ciphertext or plaintext.
func (s *PgStore) SetAppSetting(ctx context.Context, key, value string) error {
	return gen.New(s.pool).UpsertAppSetting(ctx, gen.UpsertAppSettingParams{Key: key, Value: value})
}

// SetAppSettings commits a settings change as one transaction. Values are
// already encoded by the caller; none are persisted if any upsert fails.
func (s *PgStore) SetAppSettings(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys) // Consistent lock order for overlapping provider updates.
	q := gen.New(tx)
	for _, key := range keys {
		if err := q.UpsertAppSetting(ctx, gen.UpsertAppSettingParams{Key: key, Value: values[key]}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
