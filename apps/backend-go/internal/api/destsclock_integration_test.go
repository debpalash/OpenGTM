//go:build integration

package api

import (
	"context"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

func TestCreatedDestinationTimestampsAreUTCInEverySessionTimezone(t *testing.T) {
	e := newDestEnv(t)
	ctx := context.Background()
	for _, zone := range []string{"UTC", "Asia/Kolkata", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			before := time.Now().UTC().Add(-2 * time.Second)
			var saved deststore.Destination
			err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT set_config('TimeZone', $1, true)`, zone); err != nil {
					return err
				}
				var err error
				saved, err = deststore.CreateDestination(ctx, tx, deststore.CreateInput{AudienceID: "aud-act-1", Name: "Clock " + zone, Type: "webhook", Enabled: false, Config: []byte(`{"url":"https://clock.example.test"}`)})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			after := time.Now().UTC().Add(2 * time.Second)
			for label, value := range map[string]time.Time{"created_at": saved.CreatedAt, "updated_at": saved.UpdatedAt} {
				if value.Before(before) || value.After(after) {
					t.Errorf("%s under %s is %s, outside the UTC write interval [%s, %s]", label, zone, value, before, after)
				}
			}
		})
	}
}
