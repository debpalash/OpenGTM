// Package optest provides throwaway-database fixtures for the operator
// tooling tests. Like internal/db/dbtest it is gated on
// OPENGTM_TEST_DATABASE_URL (an owner URL); every call creates a uniquely
// named database on that server and drops it at test cleanup, so tests never
// share state with each other or with the database in the URL.
package optest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/pg"
)

// Server returns a Conn for the test server (maintenance database), or skips
// the test when no database or no PostgreSQL client tools are available.
func Server(t testing.TB) pg.Conn {
	t.Helper()
	owner := dbtest.OwnerURL(t)
	for _, tool := range []string{"psql", "pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH; skipping backup/restore test", tool)
		}
	}
	c, err := pg.Direct(owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Scratch creates an empty database named opengtm_<prefix>_<random> and drops
// it when the test ends.
func Scratch(t testing.TB, server pg.Conn, prefix string) pg.Conn {
	t.Helper()
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "opengtm_" + prefix + "_" + hex.EncodeToString(b[:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := server.CreateDatabase(ctx, name)
	if err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.DropDatabase(ctx, name); err != nil {
			t.Errorf("drop scratch database %s: %v", name, err)
		}
	})
	return c
}
