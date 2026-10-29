//go:build integration

package queue_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

func TestAllowJobTypeClaimedOthersStillFail(t *testing.T) {
	pool, ctx := setup(t)
	queue.AllowJobType("allow_test_type")
	bad := insert(t, pool, "still_unsupported", "go", "a", "pending", 0, 3)
	allowed := insert(t, pool, "allow_test_type", "go", "a", "pending", 0, 3)
	c := mustClaim(t, pool, "w", 0)
	if c.ID != allowed || c.Type != "allow_test_type" {
		t.Fatalf("claimed %d (%s), want %d", c.ID, c.Type, allowed)
	}
	var st, e string
	if err := pool.QueryRow(ctx, `SELECT status, error FROM jobs WHERE id=$1`, bad).Scan(&st, &e); err != nil {
		t.Fatal(err)
	}
	if st != "failed" || e != "go executor: unsupported job type" {
		t.Fatalf("%s %q", st, e)
	}
}

func TestPermanentError(t *testing.T) {
	base := errors.New("gone")
	err := fmt.Errorf("wrapped: %w", &queue.PermanentError{Err: base})
	if !queue.IsPermanent(err) || !errors.Is(err, base) || queue.IsPermanent(base) {
		t.Fatal("IsPermanent/Unwrap misbehave")
	}
}
